package htlcswitch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/lightningnetwork/lnd/tlv"
)

func isAlias(scid lnwire.ShortChannelID) bool {
	return scid.BlockHeight >= 16_000_000 && scid.BlockHeight < 16_250_000
}

type mockPreimageCache struct {
	sync.Mutex
	preimageMap map[lntypes.Hash]lntypes.Preimage
}

func newMockPreimageCache() *mockPreimageCache {
	return &mockPreimageCache{
		preimageMap: make(map[lntypes.Hash]lntypes.Preimage),
	}
}

func (m *mockPreimageCache) LookupPreimage(
	hash lntypes.Hash) (lntypes.Preimage, bool) {

	m.Lock()
	defer m.Unlock()

	p, ok := m.preimageMap[hash]
	return p, ok
}

func (m *mockPreimageCache) AddPreimages(preimages ...lntypes.Preimage) error {
	m.Lock()
	defer m.Unlock()

	for _, preimage := range preimages {
		m.preimageMap[preimage.Hash()] = preimage
	}

	return nil
}

func (m *mockPreimageCache) SubscribeUpdates(
	chanID lnwire.ShortChannelID, htlc *channeldb.HTLC,
	payload *hop.Payload,
	nextHopOnionBlob []byte) (*contractcourt.WitnessSubscription, error) {

	return nil, nil
}

// TODO(yy): replace it with chainfee.MockEstimator.
type mockFeeEstimator struct {
	byteFeeIn chan chainfee.SatPerKWeight
	relayFee  chan chainfee.SatPerKWeight

	quit chan struct{}
}

func newMockFeeEstimator() *mockFeeEstimator {
	return &mockFeeEstimator{
		byteFeeIn: make(chan chainfee.SatPerKWeight),
		relayFee:  make(chan chainfee.SatPerKWeight),
		quit:      make(chan struct{}),
	}
}

func (m *mockFeeEstimator) EstimateFeePerKW(
	numBlocks uint32) (chainfee.SatPerKWeight, error) {

	select {
	case feeRate := <-m.byteFeeIn:
		return feeRate, nil
	case <-m.quit:
		return 0, fmt.Errorf("exiting")
	}
}

func (m *mockFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	select {
	case feeRate := <-m.relayFee:
		return feeRate
	case <-m.quit:
		return 0
	}
}

func (m *mockFeeEstimator) Start() error {
	return nil
}
func (m *mockFeeEstimator) Stop() error {
	close(m.quit)
	return nil
}

var _ chainfee.Estimator = (*mockFeeEstimator)(nil)

type mockForwardingLog struct {
	sync.Mutex

	events map[time.Time]channeldb.ForwardingEvent
}

func (m *mockForwardingLog) AddForwardingEvents(events []channeldb.ForwardingEvent) error {
	m.Lock()
	defer m.Unlock()

	for _, event := range events {
		m.events[event.Timestamp] = event
	}

	return nil
}

type mockServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.
	wg       sync.WaitGroup
	quit     chan struct{}

	t testing.TB

	name             string
	messages         chan lnwire.Message
	protocolTraceMtx sync.Mutex
	protocolTrace    []lnwire.Message

	id         [33]byte
	htlcSwitch *Switch

	registry         *mockInvoiceRegistry
	pCache           *mockPreimageCache
	interceptorFuncs []messageInterceptor
}

var _ lnpeer.Peer = (*mockServer)(nil)

func initSwitchWithDB(startingHeight uint32, db *channeldb.DB) (*Switch, error) {
	signAliasUpdate := func(u *lnwire.ChannelUpdate1) (*ecdsa.Signature,
		error) {

		return testSig, nil
	}

	cfg := Config{
		DB:                   db,
		FetchAllOpenChannels: db.ChannelStateDB().FetchAllOpenChannels,
		FetchAllChannels:     db.ChannelStateDB().FetchAllChannels,
		FetchClosedChannels:  db.ChannelStateDB().FetchClosedChannels,
		SwitchPackager:       channeldb.NewSwitchPackager(),
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
		FetchLastChannelUpdate: func(scid lnwire.ShortChannelID) (
			*lnwire.ChannelUpdate1, error) {

			return &lnwire.ChannelUpdate1{
				ShortChannelID: scid,
			}, nil
		},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		FwdEventTicker: ticker.NewForce(
			DefaultFwdEventInterval,
		),
		LogEventTicker:         ticker.NewForce(DefaultLogInterval),
		AckEventTicker:         ticker.NewForce(DefaultAckInterval),
		HtlcNotifier:           &mockHTLCNotifier{},
		Clock:                  clock.NewDefaultClock(),
		MailboxDeliveryTimeout: time.Hour,
		MaxFeeExposure:         DefaultMaxFeeExposure,
		SignAliasUpdate:        signAliasUpdate,
		IsAlias:                isAlias,
	}

	return New(cfg, startingHeight)
}

func initSwitchWithTempDB(t testing.TB, startingHeight uint32) (*Switch,
	error) {

	tempPath := filepath.Join(t.TempDir(), "switchdb")
	db := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(startingHeight, db)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func newMockServer(t testing.TB, name string, startingHeight uint32,
	db *channeldb.DB, defaultDelta uint32) (*mockServer, error) {

	var id [33]byte
	h := sha256.Sum256([]byte(name))
	copy(id[:], h[:])

	pCache := newMockPreimageCache()

	var (
		htlcSwitch *Switch
		err        error
	)
	if db == nil {
		htlcSwitch, err = initSwitchWithTempDB(t, startingHeight)
	} else {
		htlcSwitch, err = initSwitchWithDB(startingHeight, db)
	}
	if err != nil {
		return nil, err
	}

	t.Cleanup(func() { _ = htlcSwitch.Stop() })

	registry := newMockRegistry(t)

	return &mockServer{
		t:                t,
		id:               id,
		name:             name,
		messages:         make(chan lnwire.Message, 3000),
		quit:             make(chan struct{}),
		registry:         registry,
		htlcSwitch:       htlcSwitch,
		pCache:           pCache,
		interceptorFuncs: make([]messageInterceptor, 0),
	}, nil
}

func (s *mockServer) Start() error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return errors.New("mock server already started")
	}

	if err := s.htlcSwitch.Start(); err != nil {
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		defer func() {
			s.htlcSwitch.Stop()
		}()

		for {
			select {
			case msg := <-s.messages:
				s.protocolTraceMtx.Lock()
				s.protocolTrace = append(s.protocolTrace, msg)
				s.protocolTraceMtx.Unlock()

				var shouldSkip bool

				for _, interceptor := range s.interceptorFuncs {
					skip, err := interceptor(msg)
					if err != nil {
						s.t.Fatalf("%v: error in the "+
							"interceptor: %v", s.name, err)
						return
					}
					shouldSkip = shouldSkip || skip
				}

				if shouldSkip {
					continue
				}

				if err := s.readHandler(msg); err != nil {
					s.t.Fatal(err)
					return
				}
			case <-s.quit:
				return
			}
		}
	}()

	return nil
}

func (s *mockServer) QuitSignal() <-chan struct{} {
	return s.quit
}

// mockHopIterator represents the test version of hop iterator which instead
// of encrypting the path in onion blob just stores the path as a list of hops.
type mockHopIterator struct {
	hops []*hop.Payload
}

func newMockHopIterator(hops ...*hop.Payload) hop.Iterator {
	return &mockHopIterator{hops: hops}
}

func (r *mockHopIterator) HopPayload() (*hop.Payload, hop.RouteRole, error) {
	h := r.hops[0]
	r.hops = r.hops[1:]
	return h, hop.RouteRoleCleartext, nil
}

func (r *mockHopIterator) ExtraOnionBlob() []byte {
	return nil
}

func (r *mockHopIterator) ExtractErrorEncrypter(
	extracter hop.ErrorEncrypterExtracter, _ bool) (hop.ErrorEncrypter,
	lnwire.FailCode) {

	return extracter(nil)
}

func (r *mockHopIterator) EncodeNextHop(w io.Writer) error {
	var hopLength [4]byte
	binary.BigEndian.PutUint32(hopLength[:], uint32(len(r.hops)))

	if _, err := w.Write(hopLength[:]); err != nil {
		return err
	}

	for _, hop := range r.hops {
		fwdInfo := hop.ForwardingInfo()
		if err := encodeFwdInfo(w, &fwdInfo); err != nil {
			return err
		}
	}

	return nil
}

func encodeFwdInfo(w io.Writer, f *hop.ForwardingInfo) error {
	if err := binary.Write(w, binary.BigEndian, f.NextHop); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, f.AmountToForward); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, f.OutgoingCTLV); err != nil {
		return err
	}

	return nil
}

var _ hop.Iterator = (*mockHopIterator)(nil)

// mockObfuscator mock implementation of the failure obfuscator which only
// encodes the failure and do not makes any onion obfuscation.
type mockObfuscator struct {
	ogPacket *sphinx.OnionPacket
	failure  lnwire.FailureMessage
}

// NewMockObfuscator initializes a dummy mockObfuscator used for testing.
func NewMockObfuscator() hop.ErrorEncrypter {
	return &mockObfuscator{}
}

func (o *mockObfuscator) OnionPacket() *sphinx.OnionPacket {
	return o.ogPacket
}

func (o *mockObfuscator) Type() hop.EncrypterType {
	return hop.EncrypterTypeMock
}

func (o *mockObfuscator) Encode(w io.Writer) error {
	return nil
}

func (o *mockObfuscator) Decode(r io.Reader) error {
	return nil
}

func (o *mockObfuscator) Reextract(
	extracter hop.ErrorEncrypterExtracter) error {

	return nil
}

var fakeHmac = []byte("hmachmachmachmachmachmachmachmac")

func (o *mockObfuscator) EncryptFirstHop(failure lnwire.FailureMessage) (
	lnwire.OpaqueReason, error) {

	o.failure = failure

	var b bytes.Buffer
	b.Write(fakeHmac)

	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (o *mockObfuscator) IntermediateEncrypt(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return reason
}

func (o *mockObfuscator) EncryptMalformedError(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	var b bytes.Buffer
	b.Write(fakeHmac)

	b.Write(reason)

	return b.Bytes()
}

// mockDeobfuscator mock implementation of the failure deobfuscator which
// only decodes the failure do not makes any onion obfuscation.
type mockDeobfuscator struct{}

func newMockDeobfuscator() ErrorDecrypter {
	return &mockDeobfuscator{}
}

func (o *mockDeobfuscator) DecryptError(reason lnwire.OpaqueReason) (
	*ForwardingError, error) {

	if !bytes.Equal(reason[:32], fakeHmac) {
		return nil, errors.New("fake decryption error")
	}
	reason = reason[32:]

	r := bytes.NewReader(reason)
	failure, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return nil, err
	}

	return NewForwardingError(failure, 1), nil
}

var _ ErrorDecrypter = (*mockDeobfuscator)(nil)

// mockIteratorDecoder test version of hop iterator decoder which decodes the
// encoded array of hops.
type mockIteratorDecoder struct {
	mu sync.RWMutex

	responses map[[32]byte][]hop.DecodeHopIteratorResponse

	decodeFail bool
}

func newMockIteratorDecoder() *mockIteratorDecoder {
	return &mockIteratorDecoder{
		responses: make(map[[32]byte][]hop.DecodeHopIteratorResponse),
	}
}

func (p *mockIteratorDecoder) DecodeHopIterator(r io.Reader, rHash []byte,
	cltv uint32) (hop.Iterator, lnwire.FailCode) {

	var b [4]byte
	_, err := r.Read(b[:])
	if err != nil {
		return nil, lnwire.CodeTemporaryChannelFailure
	}
	hopLength := binary.BigEndian.Uint32(b[:])

	hops := make([]*hop.Payload, hopLength)
	for i := uint32(0); i < hopLength; i++ {
		var f hop.ForwardingInfo
		if err := decodeFwdInfo(r, &f); err != nil {
			return nil, lnwire.CodeTemporaryChannelFailure
		}

		var nextHopBytes [8]byte
		binary.BigEndian.PutUint64(nextHopBytes[:], f.NextHop.ToUint64())

		hops[i] = hop.NewLegacyPayload(&sphinx.HopData{
			Realm:         [1]byte{}, // hop.BitcoinNetwork
			NextAddress:   nextHopBytes,
			ForwardAmount: uint64(f.AmountToForward),
			OutgoingCltv:  f.OutgoingCTLV,
		})
	}

	return newMockHopIterator(hops...), lnwire.CodeNone
}

func (p *mockIteratorDecoder) DecodeHopIterators(id []byte,
	reqs []hop.DecodeHopIteratorRequest, _ bool) (
	[]hop.DecodeHopIteratorResponse, error) {

	idHash := sha256.Sum256(id)

	p.mu.RLock()
	if resps, ok := p.responses[idHash]; ok {
		p.mu.RUnlock()
		return resps, nil
	}
	p.mu.RUnlock()

	batchSize := len(reqs)

	resps := make([]hop.DecodeHopIteratorResponse, 0, batchSize)
	for _, req := range reqs {
		iterator, failcode := p.DecodeHopIterator(
			req.OnionReader, req.RHash, req.IncomingCltv,
		)

		if p.decodeFail {
			failcode = lnwire.CodeTemporaryChannelFailure
		}

		resp := hop.DecodeHopIteratorResponse{
			HopIterator: iterator,
			FailCode:    failcode,
		}
		resps = append(resps, resp)
	}

	p.mu.Lock()
	p.responses[idHash] = resps
	p.mu.Unlock()

	return resps, nil
}

func decodeFwdInfo(r io.Reader, f *hop.ForwardingInfo) error {
	if err := binary.Read(r, binary.BigEndian, &f.NextHop); err != nil {
		return err
	}

	if err := binary.Read(r, binary.BigEndian, &f.AmountToForward); err != nil {
		return err
	}

	if err := binary.Read(r, binary.BigEndian, &f.OutgoingCTLV); err != nil {
		return err
	}

	return nil
}

// messageInterceptor is function that handles the incoming peer messages and
// may decide should the peer skip the message or not.
type messageInterceptor func(m lnwire.Message) (bool, error)

// Record is used to set the function which will be triggered when new
// lnwire message was received.
func (s *mockServer) intersect(f messageInterceptor) {
	s.interceptorFuncs = append(s.interceptorFuncs, f)
}

func (s *mockServer) SendMessage(sync bool, msgs ...lnwire.Message) error {

	for _, msg := range msgs {
		select {
		case s.messages <- msg:
		case <-s.quit:
			return errors.New("server is stopped")
		}
	}

	return nil
}

func (s *mockServer) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	panic("not implemented")
}

func (s *mockServer) readHandler(message lnwire.Message) error {
	var targetChan lnwire.ChannelID

	switch msg := message.(type) {
	case *lnwire.UpdateAddHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFulfillHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFailHTLC:
		targetChan = msg.ChanID
	case *lnwire.UpdateFailMalformedHTLC:
		targetChan = msg.ChanID
	case *lnwire.RevokeAndAck:
		targetChan = msg.ChanID
	case *lnwire.CommitSig:
		targetChan = msg.ChanID
	case *lnwire.ChannelReady:
		// Ignore
		return nil
	case *lnwire.ChannelReestablish:
		targetChan = msg.ChanID
	case *lnwire.UpdateFee:
		targetChan = msg.ChanID
	case *lnwire.Stfu:
		targetChan = msg.ChanID
	default:
		return fmt.Errorf("unknown message type: %T", msg)
	}

	// Dispatch the commitment update message to the proper channel link
	// dedicated to this channel. If the link is not found, we will discard
	// the message.
	link, err := s.htlcSwitch.GetLink(targetChan)
	if err != nil {
		return nil
	}

	// Create goroutine for this, in order to be able to properly stop
	// the server when handler stacked (server unavailable)
	link.HandleChannelUpdate(message)

	return nil
}

func (s *mockServer) PubKey() [33]byte {
	return s.id
}

func (s *mockServer) IdentityKey() *btcec.PublicKey {
	pubkey, _ := btcec.ParsePubKey(s.id[:])
	return pubkey
}

func (s *mockServer) Address() net.Addr {
	return nil
}

func (s *mockServer) AddNewChannel(channel *lnpeer.NewChannel,
	cancel <-chan struct{}) error {

	return nil
}

func (s *mockServer) AddPendingChannel(_ lnwire.ChannelID,
	cancel <-chan struct{}) error {

	return nil
}

func (s *mockServer) RemovePendingChannel(_ lnwire.ChannelID) error {
	return nil
}

func (s *mockServer) WipeChannel(*wire.OutPoint) {}

func (s *mockServer) LocalFeatures() *lnwire.FeatureVector {
	return nil
}

func (s *mockServer) RemoteFeatures() *lnwire.FeatureVector {
	return nil
}

func (s *mockServer) Disconnect(err error) {}

func (s *mockServer) Stop() error {
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		return nil
	}

	close(s.quit)
	s.wg.Wait()

	return nil
}

func (s *mockServer) String() string {
	return s.name
}

type mockChannelLink struct {
	htlcSwitch *Switch

	shortChanID lnwire.ShortChannelID

	// Only used for zero-conf channels.
	realScid lnwire.ShortChannelID

	aliases []lnwire.ShortChannelID

	chanID lnwire.ChannelID

	peer lnpeer.Peer

	mailBox MailBox

	packets chan *htlcPacket

	eligible bool

	unadvertised bool

	zeroConf bool

	optionFeature bool

	htlcID uint64

	checkHtlcTransitResult *LinkError

	checkHtlcForwardResult *LinkError

	failAliasUpdate func(sid lnwire.ShortChannelID,
		incoming bool) *lnwire.ChannelUpdate1

	confirmedZC bool
}

// completeCircuit is a helper method for adding the finalized payment circuit
// to the switch's circuit map. In testing, this should be executed after
// receiving an htlc from the downstream packets channel.
func (f *mockChannelLink) completeCircuit(pkt *htlcPacket) error {
	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		pkt.outgoingChanID = f.shortChanID
		pkt.outgoingHTLCID = f.htlcID
		htlc.ID = f.htlcID

		keystone := Keystone{pkt.inKey(), pkt.outKey()}
		err := f.htlcSwitch.circuits.OpenCircuits(keystone)
		if err != nil {
			return err
		}

		f.htlcID++

	case *lnwire.UpdateFulfillHTLC, *lnwire.UpdateFailHTLC:
		if pkt.circuit != nil {
			err := f.htlcSwitch.teardownCircuit(pkt)
			if err != nil {
				return err
			}
		}
	}

	f.mailBox.AckPacket(pkt.inKey())

	return nil
}

func (f *mockChannelLink) deleteCircuit(pkt *htlcPacket) error {
	return f.htlcSwitch.circuits.DeleteCircuits(pkt.inKey())
}

func newMockChannelLink(htlcSwitch *Switch, chanID lnwire.ChannelID,
	shortChanID, realScid lnwire.ShortChannelID, peer lnpeer.Peer,
	eligible, unadvertised, zeroConf, optionFeature bool,
) *mockChannelLink {

	aliases := make([]lnwire.ShortChannelID, 0)
	var realConfirmed bool

	if zeroConf {
		aliases = append(aliases, shortChanID)
	}

	if realScid != hop.Source {
		realConfirmed = true
	}

	return &mockChannelLink{
		htlcSwitch:    htlcSwitch,
		chanID:        chanID,
		shortChanID:   shortChanID,
		realScid:      realScid,
		peer:          peer,
		eligible:      eligible,
		unadvertised:  unadvertised,
		zeroConf:      zeroConf,
		optionFeature: optionFeature,
		aliases:       aliases,
		confirmedZC:   realConfirmed,
	}
}

// addAlias is not part of any interface method.
func (f *mockChannelLink) addAlias(alias lnwire.ShortChannelID) {
	f.aliases = append(f.aliases, alias)
}

func (f *mockChannelLink) handleSwitchPacket(pkt *htlcPacket) error {
	f.mailBox.AddPacket(pkt)
	return nil
}

func (f *mockChannelLink) getDustSum(whoseCommit lntypes.ChannelParty,
	dryRunFee fn.Option[chainfee.SatPerKWeight]) lnwire.MilliSatoshi {

	return 0
}

func (f *mockChannelLink) getFeeRate() chainfee.SatPerKWeight {
	return 0
}

func (f *mockChannelLink) getDustClosure() dustClosure {
	dustLimit := btcutil.Amount(400)
	return dustHelper(
		channeldb.SingleFunderTweaklessBit, dustLimit, dustLimit,
	)
}

func (f *mockChannelLink) getCommitFee(remote bool) btcutil.Amount {
	return 0
}

func (f *mockChannelLink) HandleChannelUpdate(lnwire.Message) {
}

func (f *mockChannelLink) UpdateForwardingPolicy(_ models.ForwardingPolicy) {
}
func (f *mockChannelLink) CheckHtlcForward([32]byte, lnwire.MilliSatoshi,
	lnwire.MilliSatoshi, uint32, uint32, models.InboundFee, uint32,
	lnwire.ShortChannelID, lnwire.CustomRecords) *LinkError {

	return f.checkHtlcForwardResult
}

func (f *mockChannelLink) CheckHtlcTransit(payHash [32]byte,
	amt lnwire.MilliSatoshi, timeout uint32,
	heightNow uint32, _ lnwire.CustomRecords) *LinkError {

	return f.checkHtlcTransitResult
}

func (f *mockChannelLink) Stats() (
	uint64, lnwire.MilliSatoshi, lnwire.MilliSatoshi) {

	return 0, 0, 0
}

func (f *mockChannelLink) AttachMailBox(mailBox MailBox) {
	f.mailBox = mailBox
	f.packets = mailBox.PacketOutBox()
	mailBox.SetDustClosure(f.getDustClosure())
}

func (f *mockChannelLink) attachFailAliasUpdate(closure func(
	sid lnwire.ShortChannelID, incoming bool) *lnwire.ChannelUpdate1) {

	f.failAliasUpdate = closure
}

func (f *mockChannelLink) getAliases() []lnwire.ShortChannelID {
	return f.aliases
}

func (f *mockChannelLink) isZeroConf() bool {
	return f.zeroConf
}

func (f *mockChannelLink) negotiatedAliasFeature() bool {
	return f.optionFeature
}

func (f *mockChannelLink) confirmedScid() lnwire.ShortChannelID {
	return f.realScid
}

func (f *mockChannelLink) zeroConfConfirmed() bool {
	return f.confirmedZC
}

func (f *mockChannelLink) Start() error {
	f.mailBox.ResetMessages()
	f.mailBox.ResetPackets()
	return nil
}

func (f *mockChannelLink) ChanID() lnwire.ChannelID {
	return f.chanID
}

func (f *mockChannelLink) ShortChanID() lnwire.ShortChannelID {
	return f.shortChanID
}

func (f *mockChannelLink) Bandwidth() lnwire.MilliSatoshi {
	return 99999999
}

func (f *mockChannelLink) PeerPubKey() [33]byte {
	return f.peer.PubKey()
}

func (f *mockChannelLink) ChannelPoint() wire.OutPoint {
	return wire.OutPoint{}
}

func (f *mockChannelLink) Stop()                                        {}
func (f *mockChannelLink) EligibleToForward() bool                      { return f.eligible }
func (f *mockChannelLink) MayAddOutgoingHtlc(lnwire.MilliSatoshi) error { return nil }
func (f *mockChannelLink) setLiveShortChanID(sid lnwire.ShortChannelID) { f.shortChanID = sid }
func (f *mockChannelLink) IsUnadvertised() bool                         { return f.unadvertised }
func (f *mockChannelLink) UpdateShortChanID() (lnwire.ShortChannelID, error) {
	f.eligible = true
	return f.shortChanID, nil
}

func (f *mockChannelLink) EnableAdds(linkDirection LinkDirection) bool {
	// TODO(proofofkeags): Implement
	return true
}

func (f *mockChannelLink) DisableAdds(linkDirection LinkDirection) bool {
	// TODO(proofofkeags): Implement
	return true
}
func (f *mockChannelLink) IsFlushing(linkDirection LinkDirection) bool {
	// TODO(proofofkeags): Implement
	return false
}
func (f *mockChannelLink) OnFlushedOnce(func()) {
	// TODO(proofofkeags): Implement
}
func (f *mockChannelLink) OnCommitOnce(LinkDirection, func()) {
	// TODO(proofofkeags): Implement
}
func (f *mockChannelLink) InitStfu() <-chan fn.Result[lntypes.ChannelParty] {
	// TODO(proofofkeags): Implement
	c := make(chan fn.Result[lntypes.ChannelParty], 1)

	c <- fn.Errf[lntypes.ChannelParty]("InitStfu not implemented")

	return c
}

func (f *mockChannelLink) FundingCustomBlob() fn.Option[tlv.Blob] {
	return fn.None[tlv.Blob]()
}

func (f *mockChannelLink) CommitmentCustomBlob() fn.Option[tlv.Blob] {
	return fn.None[tlv.Blob]()
}

// AuxBandwidth returns the bandwidth that can be used for a channel,
// expressed in milli-satoshi. This might be different from the regular
// BTC bandwidth for custom channels. This will always return fn.None()
// for a regular (non-custom) channel.
func (f *mockChannelLink) AuxBandwidth(lnwire.MilliSatoshi,
	lnwire.ShortChannelID,
	fn.Option[tlv.Blob], AuxTrafficShaper) fn.Result[OptionalBandwidth] {

	return fn.Ok(OptionalBandwidth{})
}

var _ ChannelLink = (*mockChannelLink)(nil)

const testInvoiceCltvExpiry = 6

type mockInvoiceRegistry struct {
	settleChan chan lntypes.Hash

	registry *invoices.InvoiceRegistry
}

type mockChainNotifier struct {
	chainntnfs.ChainNotifier
}

// RegisterBlockEpochNtfn mocks a successful call to register block
// notifications.
func (m *mockChainNotifier) RegisterBlockEpochNtfn(*chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Cancel: func() {},
	}, nil
}

func newMockRegistry(t testing.TB) *mockInvoiceRegistry {
	cdb := channeldb.OpenForTesting(t, t.TempDir())

	modifierMock := &invoices.MockHtlcModifier{}
	registry := invoices.NewRegistry(
		cdb,
		invoices.NewInvoiceExpiryWatcher(
			clock.NewDefaultClock(), 0, 0, nil,
			&mockChainNotifier{},
		),
		&invoices.RegistryConfig{
			FinalCltvRejectDelta: 5,
			HtlcInterceptor:      modifierMock,
		},
	)
	registry.Start()

	return &mockInvoiceRegistry{
		registry: registry,
	}
}

func (i *mockInvoiceRegistry) LookupInvoice(ctx context.Context,
	rHash lntypes.Hash) (invoices.Invoice, error) {

	return i.registry.LookupInvoice(ctx, rHash)
}

func (i *mockInvoiceRegistry) SettleHodlInvoice(
	ctx context.Context, preimage lntypes.Preimage) error {

	return i.registry.SettleHodlInvoice(ctx, preimage)
}

func (i *mockInvoiceRegistry) NotifyExitHopHtlc(rhash lntypes.Hash,
	amt lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey models.CircuitKey, hodlChan chan<- interface{},
	wireCustomRecords lnwire.CustomRecords,
	payload invoices.Payload) (invoices.HtlcResolution, error) {

	event, err := i.registry.NotifyExitHopHtlc(
		rhash, amt, expiry, currentHeight, circuitKey,
		hodlChan, wireCustomRecords, payload,
	)
	if err != nil {
		return nil, err
	}
	if i.settleChan != nil {
		i.settleChan <- rhash
	}

	return event, nil
}

func (i *mockInvoiceRegistry) CancelInvoice(ctx context.Context,
	payHash lntypes.Hash) error {

	return i.registry.CancelInvoice(ctx, payHash)
}

func (i *mockInvoiceRegistry) AddInvoice(ctx context.Context,
	invoice invoices.Invoice, paymentHash lntypes.Hash) error {

	_, err := i.registry.AddInvoice(ctx, &invoice, paymentHash)
	return err
}

func (i *mockInvoiceRegistry) HodlUnsubscribeAll(
	subscriber chan<- interface{}) {

	i.registry.HodlUnsubscribeAll(subscriber)
}

var _ InvoiceDatabase = (*mockInvoiceRegistry)(nil)

type mockCircuitMap struct {
	lookup chan *PaymentCircuit
}

var _ CircuitMap = (*mockCircuitMap)(nil)

func (m *mockCircuitMap) OpenCircuits(...Keystone) error {
	return nil
}

func (m *mockCircuitMap) TrimOpenCircuits(chanID lnwire.ShortChannelID,
	start uint64) error {
	return nil
}

func (m *mockCircuitMap) DeleteCircuits(inKeys ...CircuitKey) error {
	return nil
}

func (m *mockCircuitMap) CommitCircuits(
	circuit ...*PaymentCircuit) (*CircuitFwdActions, error) {

	return nil, nil
}

func (m *mockCircuitMap) CloseCircuit(outKey CircuitKey) (*PaymentCircuit,
	error) {
	return nil, nil
}

func (m *mockCircuitMap) FailCircuit(inKey CircuitKey) (*PaymentCircuit,
	error) {
	return nil, nil
}

func (m *mockCircuitMap) LookupCircuit(inKey CircuitKey) *PaymentCircuit {
	return <-m.lookup
}

func (m *mockCircuitMap) LookupOpenCircuit(outKey CircuitKey) *PaymentCircuit {
	return nil
}

func (m *mockCircuitMap) LookupByPaymentHash(hash [32]byte) []*PaymentCircuit {
	return nil
}

func (m *mockCircuitMap) NumPending() int {
	return 0
}

func (m *mockCircuitMap) NumOpen() int {
	return 0
}

type mockOnionErrorDecryptor struct {
	sourceIdx int
	message   []byte
	err       error
}

func (m *mockOnionErrorDecryptor) DecryptError(encryptedData []byte) (
	*sphinx.DecryptedError, error) {

	return &sphinx.DecryptedError{
		SenderIdx: m.sourceIdx,
		Message:   m.message,
	}, m.err
}

var _ htlcNotifier = (*mockHTLCNotifier)(nil)

type mockHTLCNotifier struct {
	htlcNotifier //nolint:unused
}

func (h *mockHTLCNotifier) NotifyForwardingEvent(key HtlcKey, info HtlcInfo,
	eventType HtlcEventType) {

}

func (h *mockHTLCNotifier) NotifyLinkFailEvent(key HtlcKey, info HtlcInfo,
	eventType HtlcEventType, linkErr *LinkError,
	incoming bool) {

}

func (h *mockHTLCNotifier) NotifyForwardingFailEvent(key HtlcKey,
	eventType HtlcEventType) {

}

func (h *mockHTLCNotifier) NotifySettleEvent(key HtlcKey,
	preimage lntypes.Preimage, eventType HtlcEventType) {

}

func (h *mockHTLCNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info channeldb.FinalHtlcInfo) {

}
