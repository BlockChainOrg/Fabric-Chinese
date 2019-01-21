
//此源码被清华学神尹成大魔王专业翻译分析并修改
//尹成QQ77025077
//尹成微信18510341407
//尹成所在QQ群721929980
//尹成邮箱 yinc13@mails.tsinghua.edu.cn
//尹成毕业于清华大学,微软区块链领域全球最有价值专家
//https://mvp.microsoft.com/zh-cn/PublicProfile/4033620
/*
版权所有IBM公司。保留所有权利。

SPDX许可证标识符：Apache-2.0
**/


package discovery

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger/fabric/gossip/common"
	"github.com/hyperledger/fabric/gossip/gossip/msgstore"
	"github.com/hyperledger/fabric/gossip/util"
	proto "github.com/hyperledger/fabric/protos/gossip"
	"github.com/pkg/errors"
)

const defaultHelloInterval = time.Duration(5) * time.Second
const msgExpirationFactor = 20

var aliveExpirationCheckInterval time.Duration
var maxConnectionAttempts = 120

//setAliveTimeInterval设置活动时间间隔
func SetAliveTimeInterval(interval time.Duration) {
	util.SetVal("peer.gossip.aliveTimeInterval", interval)
}

//setAliveExpirationTimeout设置过期超时
func SetAliveExpirationTimeout(timeout time.Duration) {
	util.SetVal("peer.gossip.aliveExpirationTimeout", timeout)
	aliveExpirationCheckInterval = time.Duration(timeout / 10)
}

//setAliveExpirationCheckInterval设置过期检查间隔
func SetAliveExpirationCheckInterval(interval time.Duration) {
	aliveExpirationCheckInterval = interval
}

//setReconnectInterval设置重新连接间隔
func SetReconnectInterval(interval time.Duration) {
	util.SetVal("peer.gossip.reconnectInterval", interval)
}

//setmaxconnattempts设置最大连接数
//在调用connect（）时对等机将执行的尝试
func SetMaxConnAttempts(attempts int) {
	maxConnectionAttempts = attempts
}

type timestamp struct {
	incTime  time.Time
	seqNum   uint64
	lastSeen time.Time
}

func (ts *timestamp) String() string {
	return fmt.Sprintf("%v, %v", ts.incTime.UnixNano(), ts.seqNum)
}

type gossipDiscoveryImpl struct {
	incTime          uint64
	seqNum           uint64
	self             NetworkMember
deadLastTS       map[string]*timestamp     //H
aliveLastTS      map[string]*timestamp     //V
id2Member        map[string]*NetworkMember //所有已知成员
	aliveMembership  *util.MembershipStore
	deadMembership   *util.MembershipStore
	selfAliveMessage *proto.SignedGossipMessage

	msgStore *aliveMsgStore

	comm  CommService
	crypt CryptoService
	lock  *sync.RWMutex

	toDieChan        chan struct{}
	toDieFlag        int32
	port             int
	logger           util.Logger
	disclosurePolicy DisclosurePolicy
	pubsub           *util.PubSub

	aliveTimeInterval            time.Duration
	aliveExpirationTimeout       time.Duration
	aliveExpirationCheckInterval time.Duration
	reconnectInterval            time.Duration
}

//NewDiscoveryService返回一个新的发现服务，传递了comm模块，传递了crypto服务
func NewDiscoveryService(self NetworkMember, comm CommService, crypt CryptoService, disPol DisclosurePolicy) Discovery {
	d := &gossipDiscoveryImpl{
		self:             self,
		incTime:          uint64(time.Now().UnixNano()),
		seqNum:           uint64(0),
		deadLastTS:       make(map[string]*timestamp),
		aliveLastTS:      make(map[string]*timestamp),
		id2Member:        make(map[string]*NetworkMember),
		aliveMembership:  util.NewMembershipStore(),
		deadMembership:   util.NewMembershipStore(),
		crypt:            crypt,
		comm:             comm,
		lock:             &sync.RWMutex{},
		toDieChan:        make(chan struct{}, 1),
		toDieFlag:        int32(0),
		logger:           util.GetLogger(util.DiscoveryLogger, self.InternalEndpoint),
		disclosurePolicy: disPol,
		pubsub:           util.NewPubSub(),

		aliveTimeInterval:            getAliveTimeInterval(),
		aliveExpirationTimeout:       getAliveExpirationTimeout(),
		aliveExpirationCheckInterval: getAliveExpirationCheckInterval(),
		reconnectInterval:            getReconnectInterval(),
	}

	d.validateSelfConfig()
	d.msgStore = newAliveMsgStore(d)

	go d.periodicalSendAlive()
	go d.periodicalCheckAlive()
	go d.handleMessages()
	go d.periodicalReconnectToDead()
	go d.handlePresumedDeadPeers()

	return d
}

//查找返回网络成员，如果未找到，则返回nil
func (d *gossipDiscoveryImpl) Lookup(PKIID common.PKIidType) *NetworkMember {
	if bytes.Equal(PKIID, d.self.PKIid) {
		return &d.self
	}
	d.lock.RLock()
	defer d.lock.RUnlock()
	return copyNetworkMember(d.id2Member[string(PKIID)])
}

func (d *gossipDiscoveryImpl) Connect(member NetworkMember, id identifier) {
	for _, endpoint := range []string{member.InternalEndpoint, member.Endpoint} {
		if d.isMyOwnEndpoint(endpoint) {
			d.logger.Debug("Skipping connecting to myself")
			return
		}
	}

	d.logger.Debug("Entering", member)
	defer d.logger.Debug("Exiting")
	go func() {
		for i := 0; i < maxConnectionAttempts && !d.toDie(); i++ {
			id, err := id()
			if err != nil {
				if d.toDie() {
					return
				}
				d.logger.Warningf("Could not connect to %v : %v", member, err)
				time.Sleep(d.reconnectInterval)
				continue
			}
			peer := &NetworkMember{
				InternalEndpoint: member.InternalEndpoint,
				Endpoint:         member.Endpoint,
				PKIid:            id.ID,
			}
			m, err := d.createMembershipRequest(id.SelfOrg)
			if err != nil {
				d.logger.Warningf("Failed creating membership request: %+v", errors.WithStack(err))
				continue
			}
			req, err := m.NoopSign()
			if err != nil {
				d.logger.Warningf("Failed creating SignedGossipMessage: %+v", errors.WithStack(err))
				continue
			}
			req.Nonce = util.RandomUInt64()
			req, err = req.NoopSign()
			if err != nil {
				d.logger.Warningf("Failed adding NONCE to SignedGossipMessage %+v", errors.WithStack(err))
				continue
			}
			go d.sendUntilAcked(peer, req)
			return
		}

	}()
}

func (d *gossipDiscoveryImpl) isMyOwnEndpoint(endpoint string) bool {
	return endpoint == fmt.Sprintf("127.0.0.1:%d", d.port) || endpoint == fmt.Sprintf("localhost:%d", d.port) ||
		endpoint == d.self.InternalEndpoint || endpoint == d.self.Endpoint
}

func (d *gossipDiscoveryImpl) validateSelfConfig() {
	endpoint := d.self.InternalEndpoint
	if len(endpoint) == 0 {
		d.logger.Panic("Internal endpoint is empty:", endpoint)
	}

	internalEndpointSplit := strings.Split(endpoint, ":")
	if len(internalEndpointSplit) != 2 {
		d.logger.Panicf("Self endpoint %s isn't formatted as 'host:port'", endpoint)
	}
	myPort, err := strconv.ParseInt(internalEndpointSplit[1], 10, 64)
	if err != nil {
		d.logger.Panicf("Self endpoint %s has not valid port, %+v", endpoint, errors.WithStack(err))
	}

	if myPort > int64(math.MaxUint16) {
		d.logger.Panicf("Self endpoint %s's port takes more than 16 bits", endpoint)
	}

	d.port = int(myPort)
}

func (d *gossipDiscoveryImpl) sendUntilAcked(peer *NetworkMember, message *proto.SignedGossipMessage) {
	nonce := message.Nonce
	for i := 0; i < maxConnectionAttempts && !d.toDie(); i++ {
		sub := d.pubsub.Subscribe(fmt.Sprintf("%d", nonce), time.Second*5)
		d.comm.SendToPeer(peer, message)
		if _, timeoutErr := sub.Listen(); timeoutErr == nil {
			return
		}
		time.Sleep(d.reconnectInterval)
	}
}

func (d *gossipDiscoveryImpl) InitiateSync(peerNum int) {
	if d.toDie() {
		return
	}
	var peers2SendTo []*NetworkMember
	m, err := d.createMembershipRequest(true)
	if err != nil {
		d.logger.Warningf("Failed creating membership request: %+v", errors.WithStack(err))
		return
	}
	memReq, err := m.NoopSign()
	if err != nil {
		d.logger.Warningf("Failed creating SignedGossipMessage: %+v", errors.WithStack(err))
		return
	}
	d.lock.RLock()

	n := d.aliveMembership.Size()
	k := peerNum
	if k > n {
		k = n
	}

	aliveMembersAsSlice := d.aliveMembership.ToSlice()
	for _, i := range util.GetRandomIndices(k, n-1) {
		pulledPeer := aliveMembersAsSlice[i].GetAliveMsg().Membership
		var internalEndpoint string
		if aliveMembersAsSlice[i].Envelope.SecretEnvelope != nil {
			internalEndpoint = aliveMembersAsSlice[i].Envelope.SecretEnvelope.InternalEndpoint()
		}
		netMember := &NetworkMember{
			Endpoint:         pulledPeer.Endpoint,
			Metadata:         pulledPeer.Metadata,
			PKIid:            pulledPeer.PkiId,
			InternalEndpoint: internalEndpoint,
		}
		peers2SendTo = append(peers2SendTo, netMember)
	}

	d.lock.RUnlock()

	for _, netMember := range peers2SendTo {
		d.comm.SendToPeer(netMember, memReq)
	}
}

func (d *gossipDiscoveryImpl) handlePresumedDeadPeers() {
	defer d.logger.Debug("Stopped")

	for !d.toDie() {
		select {
		case deadPeer := <-d.comm.PresumedDead():
			if d.isAlive(deadPeer) {
				d.expireDeadMembers([]common.PKIidType{deadPeer})
			}
		case s := <-d.toDieChan:
			d.toDieChan <- s
			return
		}
	}
}

func (d *gossipDiscoveryImpl) isAlive(pkiID common.PKIidType) bool {
	d.lock.RLock()
	defer d.lock.RUnlock()
	_, alive := d.aliveLastTS[string(pkiID)]
	return alive
}

func (d *gossipDiscoveryImpl) handleMessages() {
	defer d.logger.Debug("Stopped")

	in := d.comm.Accept()
	for !d.toDie() {
		select {
		case s := <-d.toDieChan:
			d.toDieChan <- s
			return
		case m := <-in:
			d.handleMsgFromComm(m)
		}
	}
}

func (d *gossipDiscoveryImpl) handleMsgFromComm(msg proto.ReceivedMessage) {
	if msg == nil {
		return
	}
	m := msg.GetGossipMessage()
	if m.GetAliveMsg() == nil && m.GetMemRes() == nil && m.GetMemReq() == nil {
		d.logger.Warning("Got message with wrong type (expected Alive or MembershipResponse or MembershipRequest message):", m.GossipMessage)
		return
	}

	d.logger.Debug("Got message:", m)
	defer d.logger.Debug("Exiting")

	if memReq := m.GetMemReq(); memReq != nil {
		selfInfoGossipMsg, err := memReq.SelfInformation.ToGossipMessage()
		if err != nil {
			d.logger.Warningf("Failed deserializing GossipMessage from envelope: %+v", errors.WithStack(err))
			return
		}

		if !d.crypt.ValidateAliveMsg(selfInfoGossipMsg) {
			return
		}

		if d.msgStore.CheckValid(selfInfoGossipMsg) {
			d.handleAliveMessage(selfInfoGossipMsg)
		}

		var internalEndpoint string
		if m.Envelope.SecretEnvelope != nil {
			internalEndpoint = m.Envelope.SecretEnvelope.InternalEndpoint()
		}

//向对等端发送成员响应可能会阻止此例程
//以防发送故意缓慢（即攻击）。
//将保持此异步，直到我在通信层中写入超时检测器
		go d.sendMemResponse(selfInfoGossipMsg.GetAliveMsg().Membership, internalEndpoint, m.Nonce)
		return
	}

	if m.IsAliveMsg() {
		if !d.msgStore.CheckValid(m) || !d.crypt.ValidateAliveMsg(m) {
			return
		}
//如果消息是我发的，忽略它，不要再转发了
		if d.isSentByMe(m) {
			return
		}

		d.msgStore.Add(m)
		d.handleAliveMessage(m)
		d.comm.Forward(msg)
		return
	}

	if memResp := m.GetMemRes(); memResp != nil {
		d.pubsub.Publish(fmt.Sprintf("%d", m.Nonce), m.Nonce)
		for _, env := range memResp.Alive {
			am, err := env.ToGossipMessage()
			if err != nil {
				d.logger.Warningf("Membership response contains an invalid message from an online peer:%+v", errors.WithStack(err))
				return
			}
			if !am.IsAliveMsg() {
				d.logger.Warning("Expected alive message, got", am, "instead")
				return
			}

			if d.msgStore.CheckValid(am) && d.crypt.ValidateAliveMsg(am) {
				d.handleAliveMessage(am)
			}
		}

		for _, env := range memResp.Dead {
			dm, err := env.ToGossipMessage()
			if err != nil {
				d.logger.Warningf("Membership response contains an invalid message from an offline peer %+v", errors.WithStack(err))
				return
			}

//存在更新的活动消息或消息不真实
			if !d.msgStore.CheckValid(dm) || !d.crypt.ValidateAliveMsg(dm) {
				continue
			}

			newDeadMembers := []*proto.SignedGossipMessage{}
			d.lock.RLock()
			if _, known := d.id2Member[string(dm.GetAliveMsg().Membership.PkiId)]; !known {
				newDeadMembers = append(newDeadMembers, dm)
			}
			d.lock.RUnlock()
			d.learnNewMembers([]*proto.SignedGossipMessage{}, newDeadMembers)
		}
	}
}

func (d *gossipDiscoveryImpl) sendMemResponse(targetMember *proto.Member, internalEndpoint string, nonce uint64) {
	d.logger.Debug("Entering", targetMember)

	targetPeer := &NetworkMember{
		Endpoint:         targetMember.Endpoint,
		Metadata:         targetMember.Metadata,
		PKIid:            targetMember.PkiId,
		InternalEndpoint: internalEndpoint,
	}

	var aliveMsg *proto.SignedGossipMessage
	var err error
	d.lock.RLock()
	aliveMsg = d.selfAliveMessage
	d.lock.RUnlock()
	if aliveMsg == nil {
		aliveMsg, err = d.createSignedAliveMessage(true)
		if err != nil {
			d.logger.Warningf("Failed creating alive message: %+v", errors.WithStack(err))
			return
		}
	}
	memResp := d.createMembershipResponse(aliveMsg, targetPeer)
	if memResp == nil {
		errMsg := `Got a membership request from a peer that shouldn't have sent one: %v, closing connection to the peer as a result.`
		d.logger.Warningf(errMsg, targetMember)
		d.comm.CloseConn(targetPeer)
		return
	}

	defer d.logger.Debug("Exiting, replying with", memResp)

	msg, err := (&proto.GossipMessage{
		Tag:   proto.GossipMessage_EMPTY,
		Nonce: nonce,
		Content: &proto.GossipMessage_MemRes{
			MemRes: memResp,
		},
	}).NoopSign()
	if err != nil {
		err = errors.WithStack(err)
		d.logger.Warningf("Failed creating SignedGossipMessage: %+v", errors.WithStack(err))
		return
	}
	d.comm.SendToPeer(targetPeer, msg)
}

func (d *gossipDiscoveryImpl) createMembershipResponse(aliveMsg *proto.SignedGossipMessage, targetMember *NetworkMember) *proto.MembershipResponse {
	shouldBeDisclosed, omitConcealedFields := d.disclosurePolicy(targetMember)

	if !shouldBeDisclosed(aliveMsg) {
		return nil
	}

	d.lock.RLock()
	defer d.lock.RUnlock()

	deadPeers := []*proto.Envelope{}

	for _, dm := range d.deadMembership.ToSlice() {

		if !shouldBeDisclosed(dm) {
			continue
		}
		deadPeers = append(deadPeers, omitConcealedFields(dm))
	}

	var aliveSnapshot []*proto.Envelope
	for _, am := range d.aliveMembership.ToSlice() {
		if !shouldBeDisclosed(am) {
			continue
		}
		aliveSnapshot = append(aliveSnapshot, omitConcealedFields(am))
	}

	return &proto.MembershipResponse{
		Alive: append(aliveSnapshot, omitConcealedFields(aliveMsg)),
		Dead:  deadPeers,
	}
}

func (d *gossipDiscoveryImpl) handleAliveMessage(m *proto.SignedGossipMessage) {
	d.logger.Debug("Entering", m)
	defer d.logger.Debug("Exiting")

	if d.isSentByMe(m) {
		return
	}

	pkiID := m.GetAliveMsg().Membership.PkiId

	ts := m.GetAliveMsg().Timestamp

	d.lock.RLock()
	_, known := d.id2Member[string(pkiID)]
	d.lock.RUnlock()

	if !known {
		d.learnNewMembers([]*proto.SignedGossipMessage{m}, []*proto.SignedGossipMessage{})
		return
	}

	d.lock.RLock()
	_, isAlive := d.aliveLastTS[string(pkiID)]
	lastDeadTS, isDead := d.deadLastTS[string(pkiID)]
	d.lock.RUnlock()

	if !isAlive && !isDead {
		d.logger.Panicf("Member %s is known but not found neither in alive nor in dead lastTS maps, isAlive=%v, isDead=%v", m.GetAliveMsg().Membership.Endpoint, isAlive, isDead)
		return
	}

	if isAlive && isDead {
		d.logger.Panicf("Member %s is both alive and dead at the same time", m.GetAliveMsg().Membership)
		return
	}

	if isDead {
		if before(lastDeadTS, ts) {
//复活同伴
			d.resurrectMember(m, *ts)
		} else if !same(lastDeadTS, ts) {
			d.logger.Debug(m.GetAliveMsg().Membership, "lastDeadTS:", lastDeadTS, "but got ts:", ts)
		}
		return
	}

	d.lock.RLock()
	lastAliveTS, isAlive := d.aliveLastTS[string(pkiID)]
	d.lock.RUnlock()

	if isAlive {
		if before(lastAliveTS, ts) {
			d.learnExistingMembers([]*proto.SignedGossipMessage{m})
		} else if !same(lastAliveTS, ts) {
			d.logger.Debug(m.GetAliveMsg().Membership, "lastAliveTS:", lastAliveTS, "but got ts:", ts)
		}

	}
//否则，忽略消息，因为它太旧了
}

func (d *gossipDiscoveryImpl) isSentByMe(m *proto.SignedGossipMessage) bool {
	pkiID := m.GetAliveMsg().Membership.PkiId
	if !equalPKIid(pkiID, d.self.PKIid) {
		return false
	}
	d.logger.Debug("Got alive message about ourselves,", m)
	diffExternalEndpoint := d.self.Endpoint != m.GetAliveMsg().Membership.Endpoint
	var diffInternalEndpoint bool
	secretEnvelope := m.GetSecretEnvelope()
	if secretEnvelope != nil && secretEnvelope.InternalEndpoint() != "" {
		diffInternalEndpoint = secretEnvelope.InternalEndpoint() != d.self.InternalEndpoint
	}
	if diffInternalEndpoint || diffExternalEndpoint {
		d.logger.Error("Bad configuration detected: Received AliveMessage from a peer with the same PKI-ID as myself:", m.GossipMessage)
	}
	return true
}

func (d *gossipDiscoveryImpl) resurrectMember(am *proto.SignedGossipMessage, t proto.PeerTime) {
	d.logger.Debug("Entering, AliveMessage:", am, "t:", t)
	defer d.logger.Debug("Exiting")
	d.lock.Lock()
	defer d.lock.Unlock()

	member := am.GetAliveMsg().Membership
	pkiID := member.PkiId
	d.aliveLastTS[string(pkiID)] = &timestamp{
		lastSeen: time.Now(),
		seqNum:   t.SeqNum,
		incTime:  tsToTime(t.IncNum),
	}

	var internalEndpoint string
	if prevNetMem := d.id2Member[string(pkiID)]; prevNetMem != nil {
		internalEndpoint = prevNetMem.InternalEndpoint
	}
	if am.Envelope.SecretEnvelope != nil {
		internalEndpoint = am.Envelope.SecretEnvelope.InternalEndpoint()
	}

	d.id2Member[string(pkiID)] = &NetworkMember{
		Endpoint:         member.Endpoint,
		Metadata:         member.Metadata,
		PKIid:            member.PkiId,
		InternalEndpoint: internalEndpoint,
	}

	delete(d.deadLastTS, string(pkiID))
	d.deadMembership.Remove(common.PKIidType(pkiID))
	d.aliveMembership.Put(common.PKIidType(pkiID), &proto.SignedGossipMessage{GossipMessage: am.GossipMessage, Envelope: am.Envelope})
}

func (d *gossipDiscoveryImpl) periodicalReconnectToDead() {
	defer d.logger.Debug("Stopped")

	for !d.toDie() {
		wg := &sync.WaitGroup{}

		for _, member := range d.copyLastSeen(d.deadLastTS) {
			wg.Add(1)
			go func(member NetworkMember) {
				defer wg.Done()
				if d.comm.Ping(&member) {
					d.logger.Debug(member, "is responding, sending membership request")
					d.sendMembershipRequest(&member, true)
				} else {
					d.logger.Debug(member, "is still dead")
				}
			}(member)
		}

		wg.Wait()
		d.logger.Debug("Sleeping", d.reconnectInterval)
		time.Sleep(d.reconnectInterval)
	}
}

func (d *gossipDiscoveryImpl) sendMembershipRequest(member *NetworkMember, includeInternalEndpoint bool) {
	m, err := d.createMembershipRequest(includeInternalEndpoint)
	if err != nil {
		d.logger.Warningf("Failed creating membership request: %+v", errors.WithStack(err))
		return
	}
	req, err := m.NoopSign()
	if err != nil {
		d.logger.Errorf("Failed creating SignedGossipMessage: %+v", errors.WithStack(err))
		return
	}
	d.comm.SendToPeer(member, req)
}

func (d *gossipDiscoveryImpl) createMembershipRequest(includeInternalEndpoint bool) (*proto.GossipMessage, error) {
	am, err := d.createSignedAliveMessage(includeInternalEndpoint)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	req := &proto.MembershipRequest{
		SelfInformation: am.Envelope,
//TODO:发送已知对等端不安全，因为远程对等端可能不应该知道
//托多：关于已知的同龄人。在实现安全机制之前，我不赞成这样做。
//TODO:有关跟踪此问题的信息，请参阅FAB-2570。
		Known: [][]byte{},
	}
	return &proto.GossipMessage{
		Tag:   proto.GossipMessage_EMPTY,
		Nonce: uint64(0),
		Content: &proto.GossipMessage_MemReq{
			MemReq: req,
		},
	}, nil
}

func (d *gossipDiscoveryImpl) copyLastSeen(lastSeenMap map[string]*timestamp) []NetworkMember {
	d.lock.RLock()
	defer d.lock.RUnlock()

	res := []NetworkMember{}
	for pkiIDStr := range lastSeenMap {
		res = append(res, *(d.id2Member[pkiIDStr]))
	}
	return res
}

func (d *gossipDiscoveryImpl) periodicalCheckAlive() {
	defer d.logger.Debug("Stopped")

	for !d.toDie() {
		time.Sleep(d.aliveExpirationCheckInterval)
		dead := d.getDeadMembers()
		if len(dead) > 0 {
			d.logger.Debugf("Got %v dead members: %v", len(dead), dead)
			d.expireDeadMembers(dead)
		}
	}
}

func (d *gossipDiscoveryImpl) expireDeadMembers(dead []common.PKIidType) {
	d.logger.Warning("Entering", dead)
	defer d.logger.Warning("Exiting")

	var deadMembers2Expire []*NetworkMember

	d.lock.Lock()

	for _, pkiID := range dead {
		if _, isAlive := d.aliveLastTS[string(pkiID)]; !isAlive {
			continue
		}
		deadMembers2Expire = append(deadMembers2Expire, d.id2Member[string(pkiID)])
//把最后一个从活的移到死的
		lastTS, hasLastTS := d.aliveLastTS[string(pkiID)]
		if hasLastTS {
			d.deadLastTS[string(pkiID)] = lastTS
			delete(d.aliveLastTS, string(pkiID))
		}

		if am := d.aliveMembership.MsgByID(pkiID); am != nil {
			d.deadMembership.Put(pkiID, am)
			d.aliveMembership.Remove(pkiID)
		}
	}

	d.lock.Unlock()

	for _, member2Expire := range deadMembers2Expire {
		d.logger.Warning("Closing connection to", member2Expire)
		d.comm.CloseConn(member2Expire)
	}
}

func (d *gossipDiscoveryImpl) getDeadMembers() []common.PKIidType {
	d.lock.RLock()
	defer d.lock.RUnlock()

	dead := []common.PKIidType{}
	for id, last := range d.aliveLastTS {
		elapsedNonAliveTime := time.Since(last.lastSeen)
		if elapsedNonAliveTime > d.aliveExpirationTimeout {
			d.logger.Warning("Haven't heard from", []byte(id), "for", elapsedNonAliveTime)
			dead = append(dead, common.PKIidType(id))
		}
	}
	return dead
}

func (d *gossipDiscoveryImpl) periodicalSendAlive() {
	defer d.logger.Debug("Stopped")

	for !d.toDie() {
		d.logger.Debug("Sleeping", d.aliveTimeInterval)
		time.Sleep(d.aliveTimeInterval)
		msg, err := d.createSignedAliveMessage(true)
		if err != nil {
			d.logger.Warningf("Failed creating alive message: %+v", errors.WithStack(err))
			return
		}
		d.lock.Lock()
		d.selfAliveMessage = msg
		d.lock.Unlock()
		d.comm.Gossip(msg)
	}
}

func (d *gossipDiscoveryImpl) aliveMsgAndInternalEndpoint() (*proto.GossipMessage, string) {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.seqNum++
	seqNum := d.seqNum
	endpoint := d.self.Endpoint
	meta := d.self.Metadata
	pkiID := d.self.PKIid
	internalEndpoint := d.self.InternalEndpoint
	msg := &proto.GossipMessage{
		Tag: proto.GossipMessage_EMPTY,
		Content: &proto.GossipMessage_AliveMsg{
			AliveMsg: &proto.AliveMessage{
				Membership: &proto.Member{
					Endpoint: endpoint,
					Metadata: meta,
					PkiId:    pkiID,
				},
				Timestamp: &proto.PeerTime{
					IncNum: uint64(d.incTime),
					SeqNum: seqNum,
				},
			},
		},
	}
	return msg, internalEndpoint
}

func (d *gossipDiscoveryImpl) createSignedAliveMessage(includeInternalEndpoint bool) (*proto.SignedGossipMessage, error) {
	msg, internalEndpoint := d.aliveMsgAndInternalEndpoint()
	envp := d.crypt.SignMessage(msg, internalEndpoint)
	if envp == nil {
		return nil, errors.New("Failed signing message")
	}
	signedMsg := &proto.SignedGossipMessage{
		GossipMessage: msg,
		Envelope:      envp,
	}

	if !includeInternalEndpoint {
		signedMsg.Envelope.SecretEnvelope = nil
	}

	return signedMsg, nil
}

func (d *gossipDiscoveryImpl) learnExistingMembers(aliveArr []*proto.SignedGossipMessage) {
	d.logger.Debugf("Entering: learnedMembers={%v}", aliveArr)
	defer d.logger.Debug("Exiting")

	d.lock.Lock()
	defer d.lock.Unlock()

	for _, m := range aliveArr {
		am := m.GetAliveMsg()
		if am == nil {
			d.logger.Warning("Expected alive message, got instead:", m)
			return
		}
		d.logger.Debug("updating", am)

		var internalEndpoint string
		if prevNetMem := d.id2Member[string(am.Membership.PkiId)]; prevNetMem != nil {
			internalEndpoint = prevNetMem.InternalEndpoint
		}
		if m.Envelope.SecretEnvelope != nil {
			internalEndpoint = m.Envelope.SecretEnvelope.InternalEndpoint()
		}

//更新成员的数据
		member := d.id2Member[string(am.Membership.PkiId)]
		member.Endpoint = am.Membership.Endpoint
		member.Metadata = am.Membership.Metadata
		member.InternalEndpoint = internalEndpoint

		if _, isKnownAsDead := d.deadLastTS[string(am.Membership.PkiId)]; isKnownAsDead {
			d.logger.Warning(am.Membership, "has already expired")
			continue
		}

		if _, isKnownAsAlive := d.aliveLastTS[string(am.Membership.PkiId)]; !isKnownAsAlive {
			d.logger.Warning(am.Membership, "has already expired")
			continue
		} else {
			d.logger.Debug("Updating aliveness data:", am)
//更新现有有效性数据
			alive := d.aliveLastTS[string(am.Membership.PkiId)]
			alive.incTime = tsToTime(am.Timestamp.IncNum)
			alive.lastSeen = time.Now()
			alive.seqNum = am.Timestamp.SeqNum

			if am := d.aliveMembership.MsgByID(m.GetAliveMsg().Membership.PkiId); am == nil {
				d.logger.Debug("Adding", am, "to aliveMembership")
				msg := &proto.SignedGossipMessage{GossipMessage: m.GossipMessage, Envelope: am.Envelope}
				d.aliveMembership.Put(m.GetAliveMsg().Membership.PkiId, msg)
			} else {
				d.logger.Debug("Replacing", am, "in aliveMembership")
				am.GossipMessage = m.GossipMessage
				am.Envelope = m.Envelope
			}
		}
	}
}

func (d *gossipDiscoveryImpl) learnNewMembers(aliveMembers []*proto.SignedGossipMessage, deadMembers []*proto.SignedGossipMessage) {
	d.logger.Debugf("Entering: learnedMembers={%v}, deadMembers={%v}", aliveMembers, deadMembers)
	defer d.logger.Debugf("Exiting")

	d.lock.Lock()
	defer d.lock.Unlock()

	for _, am := range aliveMembers {
		if equalPKIid(am.GetAliveMsg().Membership.PkiId, d.self.PKIid) {
			continue
		}
		d.aliveLastTS[string(am.GetAliveMsg().Membership.PkiId)] = &timestamp{
			incTime:  tsToTime(am.GetAliveMsg().Timestamp.IncNum),
			lastSeen: time.Now(),
			seqNum:   am.GetAliveMsg().Timestamp.SeqNum,
		}

		d.aliveMembership.Put(am.GetAliveMsg().Membership.PkiId, &proto.SignedGossipMessage{GossipMessage: am.GossipMessage, Envelope: am.Envelope})
		d.logger.Debugf("Learned about a new alive member: %v", am)
	}

	for _, dm := range deadMembers {
		if equalPKIid(dm.GetAliveMsg().Membership.PkiId, d.self.PKIid) {
			continue
		}
		d.deadLastTS[string(dm.GetAliveMsg().Membership.PkiId)] = &timestamp{
			incTime:  tsToTime(dm.GetAliveMsg().Timestamp.IncNum),
			lastSeen: time.Now(),
			seqNum:   dm.GetAliveMsg().Timestamp.SeqNum,
		}

		d.deadMembership.Put(dm.GetAliveMsg().Membership.PkiId, &proto.SignedGossipMessage{GossipMessage: dm.GossipMessage, Envelope: dm.Envelope})
		d.logger.Debugf("Learned about a new dead member: %v", dm)
	}

//在任何情况下更新成员
	for _, a := range [][]*proto.SignedGossipMessage{aliveMembers, deadMembers} {
		for _, m := range a {
			member := m.GetAliveMsg()
			if member == nil {
				d.logger.Warning("Expected alive message, got instead:", m)
				return
			}

			var internalEndpoint string
			if m.Envelope.SecretEnvelope != nil {
				internalEndpoint = m.Envelope.SecretEnvelope.InternalEndpoint()
			}

			if prevNetMem := d.id2Member[string(member.Membership.PkiId)]; prevNetMem != nil {
				internalEndpoint = prevNetMem.InternalEndpoint
			}

			d.id2Member[string(member.Membership.PkiId)] = &NetworkMember{
				Endpoint:         member.Membership.Endpoint,
				Metadata:         member.Membership.Metadata,
				PKIid:            member.Membership.PkiId,
				InternalEndpoint: internalEndpoint,
			}
		}
	}
}

func (d *gossipDiscoveryImpl) GetMembership() []NetworkMember {
	if d.toDie() {
		return []NetworkMember{}
	}
	d.lock.RLock()
	defer d.lock.RUnlock()

	response := []NetworkMember{}
	for _, m := range d.aliveMembership.ToSlice() {
		member := m.GetAliveMsg()
		response = append(response, NetworkMember{
			PKIid:            member.Membership.PkiId,
			Endpoint:         member.Membership.Endpoint,
			Metadata:         member.Membership.Metadata,
			InternalEndpoint: d.id2Member[string(m.GetAliveMsg().Membership.PkiId)].InternalEndpoint,
			Envelope:         m.Envelope,
		})
	}
	return response

}

func tsToTime(ts uint64) time.Time {
	return time.Unix(int64(0), int64(ts))
}

func (d *gossipDiscoveryImpl) UpdateMetadata(md []byte) {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.self.Metadata = md
}

func (d *gossipDiscoveryImpl) UpdateEndpoint(endpoint string) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.self.Endpoint = endpoint
}

func (d *gossipDiscoveryImpl) Self() NetworkMember {
	var env *proto.Envelope
	msg, _ := d.aliveMsgAndInternalEndpoint()
	sMsg, err := msg.NoopSign()
	if err != nil {
		d.logger.Warning("Failed creating SignedGossipMessage:", err)
	} else {
		env = sMsg.Envelope
	}
	mem := msg.GetAliveMsg().Membership
	return NetworkMember{
		Endpoint: mem.Endpoint,
		Metadata: mem.Metadata,
		PKIid:    mem.PkiId,
		Envelope: env,
	}
}

func (d *gossipDiscoveryImpl) toDie() bool {
	toDie := atomic.LoadInt32(&d.toDieFlag) == int32(1)
	return toDie
}

func (d *gossipDiscoveryImpl) Stop() {
	defer d.logger.Info("Stopped")
	d.logger.Info("Stopping")
	atomic.StoreInt32(&d.toDieFlag, int32(1))
	d.msgStore.Stop()
	d.toDieChan <- struct{}{}
}

func copyNetworkMember(member *NetworkMember) *NetworkMember {
	if member == nil {
		return nil
	} else {
		copiedNetworkMember := &NetworkMember{}
		*copiedNetworkMember = *member
		return copiedNetworkMember
	}
}

func equalPKIid(a, b common.PKIidType) bool {
	return bytes.Equal(a, b)
}

func same(a *timestamp, b *proto.PeerTime) bool {
	return uint64(a.incTime.UnixNano()) == b.IncNum && a.seqNum == b.SeqNum
}

func before(a *timestamp, b *proto.PeerTime) bool {
	return (uint64(a.incTime.UnixNano()) == b.IncNum && a.seqNum < b.SeqNum) ||
		uint64(a.incTime.UnixNano()) < b.IncNum
}

func getAliveTimeInterval() time.Duration {
	return util.GetDurationOrDefault("peer.gossip.aliveTimeInterval", defaultHelloInterval)
}

func getAliveExpirationTimeout() time.Duration {
	return util.GetDurationOrDefault("peer.gossip.aliveExpirationTimeout", 5*getAliveTimeInterval())
}

func getAliveExpirationCheckInterval() time.Duration {
	if aliveExpirationCheckInterval != 0 {
		return aliveExpirationCheckInterval
	}

	return time.Duration(getAliveExpirationTimeout() / 10)
}

func getReconnectInterval() time.Duration {
	return util.GetDurationOrDefault("peer.gossip.reconnectInterval", getAliveExpirationTimeout())
}

type aliveMsgStore struct {
	msgstore.MessageStore
}

func newAliveMsgStore(d *gossipDiscoveryImpl) *aliveMsgStore {
	policy := proto.NewGossipMessageComparator(0)
	trigger := func(m interface{}) {}
	aliveMsgTTL := d.aliveExpirationTimeout * msgExpirationFactor
	externalLock := func() { d.lock.Lock() }
	externalUnlock := func() { d.lock.Unlock() }
	callback := func(m interface{}) {
		msg := m.(*proto.SignedGossipMessage)
		if !msg.IsAliveMsg() {
			return
		}
		id := msg.GetAliveMsg().Membership.PkiId
		d.aliveMembership.Remove(id)
		d.deadMembership.Remove(id)
		delete(d.id2Member, string(id))
		delete(d.deadLastTS, string(id))
		delete(d.aliveLastTS, string(id))
	}

	s := &aliveMsgStore{
		MessageStore: msgstore.NewMessageStoreExpirable(policy, trigger, aliveMsgTTL, externalLock, externalUnlock, callback),
	}
	return s
}

func (s *aliveMsgStore) Add(msg interface{}) bool {
	if !msg.(*proto.SignedGossipMessage).IsAliveMsg() {
		panic(fmt.Sprint("Msg ", msg, " is not AliveMsg"))
	}
	return s.MessageStore.Add(msg)
}

func (s *aliveMsgStore) CheckValid(msg interface{}) bool {
	if !msg.(*proto.SignedGossipMessage).IsAliveMsg() {
		panic(fmt.Sprint("Msg ", msg, " is not AliveMsg"))
	}
	return s.MessageStore.CheckValid(msg)
}