// Package iniva implements proposal dissemination and vote aggregation
package iniva

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"reflect"
	"sort"
	"time"

	"github.com/relab/gorums"
	"github.com/relab/hotstuff"
	"github.com/relab/hotstuff/backend"
	"github.com/relab/hotstuff/eventloop"
	"github.com/relab/hotstuff/internal/proto/hotstuffpb"
	"github.com/relab/hotstuff/internal/proto/inivapb"
	"github.com/relab/hotstuff/logging"
	"github.com/relab/hotstuff/modules"
)

func init() {
	modules.RegisterModule("iniva", New)
}

const (
	contributionWait = 800 * time.Millisecond
	leaderWait       = 50 * time.Millisecond
)

// Iniva implements a signature aggregation protocol.
type Iniva struct {
	//sync.Mutex
	configuration          *backend.Config
	server                 *backend.Server
	blockChain             modules.BlockChain
	crypto                 modules.Crypto
	eventLoop              *eventloop.EventLoop
	logger                 logging.Logger
	opts                   *modules.Options
	leaderRotation         modules.LeaderRotation
	synchronizer           modules.Synchronizer
	nodes                  map[hotstuff.ID]*inivapb.Node
	tree                   *TreeConfiguration
	initDone               bool
	beginDone              bool
	aggregatedContribution hotstuff.QuorumSignature
	ProposalMsg            hotstuff.ProposeMsg
	children               []hotstuff.ID
	senders                []hotstuff.ID
	cancel                 context.CancelFunc
	currentView            hotstuff.View
	inSecondChance         bool
}

// New returns a new instance of the Handel module.
func New() modules.Iniva {

	return &Iniva{
		nodes:   make(map[hotstuff.ID]*inivapb.Node),
		senders: make([]hotstuff.ID, 0),
	}
}

// InitModule initializes the iniva module
func (r *Iniva) InitModule(mods *modules.Core) {
	mods.Get(
		&r.configuration,
		&r.server,
		&r.blockChain,
		&r.crypto,
		&r.eventLoop,
		&r.logger,
		&r.opts,
		&r.leaderRotation,
		&r.synchronizer,
	)
	r.opts.SetShouldUseIniva()
	r.eventLoop.RegisterObserver(backend.ConnectedEvent{}, func(_ any) {
		r.postInit()
	})
	// register handler for contribution and timeout event
	r.eventLoop.RegisterHandler(ContributionRecvEvent{}, func(event any) {
		r.OnContributionRecv(event.(ContributionRecvEvent))
	})
	r.eventLoop.RegisterHandler(ACKRecvEvent{}, func(event any) {
		r.OnACK(event.(ACKRecvEvent))
	})
}

func (r *Iniva) postInit() {

	InivaCfg := inivapb.ConfigurationFromRaw(r.configuration.GetRawConfiguration(), nil)
	for _, n := range InivaCfg.Nodes() {
		r.nodes[hotstuff.ID(n.ID())] = n
	}
	inivapb.RegisterInivaServer(r.server.GetGorumsServer(), serviceImpl{r})
	r.tree = CreateTree(r.configuration.Len(), r.opts.ID())
	r.initDone = true
	r.logger.Debug("Iniva: Configuration initialization completed")
}

// Begin starts dissemination and aggregation process
func (r *Iniva) Begin(s hotstuff.PartialCert, p hotstuff.ProposeMsg) {
	r.logger.Debug("Received proposal from ", p.ID)
	if !r.initDone {
		// wait until initialization is done
		r.logger.Debug("Waiting for the initialization")
		//r.eventLoop.DelayUntil(backend.ConnectedEvent{}, func() { r.Begin(s, p, v) })
		r.postInit()
	}
	if p.SecondChance {
		r.logger.Debug("Received second chance")
		//Already seen the proposal, so sending aggregated signature
		if r.currentView == p.Block.QuorumCert().View() {
			r.SendContributionToNode(p.Block.Proposer(), r.aggregatedContribution)
		} else {
			//Sending vote to the leader, no tree construction
			r.SendContributionToNode(p.Block.Proposer(), s.Signature())
		}
		return
	}
	r.reset()
	r.beginDone = true
	r.ProposalMsg = p
	r.currentView = p.Block.View()
	r.aggregatedContribution = s.Signature()
	idMappings := r.randomizeIDs(p.Block.Hash(), r.leaderRotation.GetLeader(r.ProposalMsg.Block.View()))
	r.logger.Debug("id mappings are ", idMappings)
	// To test second chance.
	// if idMappings[r.opts.ID()] == r.configuration.Len()-1 {
	// 	return
	// }
	r.tree.InitializeWithPIDs(idMappings)
	r.children = r.tree.GetChildren()
	if len(r.children) == 0 {
		parent, ok := r.tree.GetParent()
		if ok {
			r.SendContributionToNode(parent, s.Signature())
		}
	} else {
		r.sendProposalToChildren(p)
	}
}

// OnContributionRecv handles the incoming contributions
func (r *Iniva) OnContributionRecv(event ContributionRecvEvent) {
	if !r.beginDone || event.Contribution.View != uint64(r.ProposalMsg.Block.View()) {
		r.logger.Debug("Contribution from ", event.Contribution, "  is ignored for view ", r.ProposalMsg.Block.View())
		return
	}
	contribution := event.Contribution
	r.logger.Debug("processing the contribution from ", contribution.ID)
	currentSignature := hotstuffpb.QuorumSignatureFromProto(contribution.Signature)
	err := r.mergeWithContribution(currentSignature)
	if err != nil {
		r.logger.Info("Unable to merge the contribution from ", event.Contribution.ID,
			event.Contribution.View)
		return
	}
	r.senders = append(r.senders, hotstuff.ID(contribution.ID))
	//In second chance cancel only if all replicas replied
	if r.inSecondChance && r.aggregatedContribution.Participants().Len() == r.configuration.Len() {
		r.logger.Debug("Completed aggregation in second chance ")
		r.cancel()
	}
	//in normal case cancel if all children replied
	if !r.inSecondChance && isSubSet(r.children, r.senders) {
		r.logger.Debug("Completed aggregation ")
		r.cancel()
	}

}

func (r *Iniva) performSecondChance() {
	r.logger.Debug("Performing second chance ")
	r.inSecondChance = true
	signaturesPresent := make([]hotstuff.ID, 0)
	r.aggregatedContribution.Participants().ForEach(func(id hotstuff.ID) {
		signaturesPresent = append(signaturesPresent, id)
	})
	absent := make([]hotstuff.ID, 0)
	for id := range r.configuration.Replicas() {
		found := false
		for _, pID := range signaturesPresent {
			if id == pID {
				found = true
				break
			}
		}
		if !found {
			absent = append(absent, id)
		}
	}
	subConfig, err := r.configuration.SubConfig(absent)
	if err != nil {
		r.logger.Warn("Unable to create configuration")
		return
	}
	proposal := r.ProposalMsg
	proposal.SecondChance = true
	subConfig.Propose(proposal)
	context, cancel := context.WithTimeout(context.Background(), leaderWait)
	r.cancel = cancel
	go func() {
		<-context.Done()
		r.synchronizer.AdvanceView(hotstuff.NewSyncInfo().WithQC(
			hotstuff.NewQuorumCert(r.aggregatedContribution, r.ProposalMsg.Block.View(),
				r.ProposalMsg.Block.Hash())))
	}()
}

func (r *Iniva) sendProposalToChildren(proposal hotstuff.ProposeMsg) {

	r.logger.Debug("sending proposal to children ", r.children, proposal)
	config, err := r.configuration.SubConfig(r.children)
	if err != nil {
		r.logger.Error("Unable to send the proposal to children", err)
		return
	}
	config.Propose(proposal)
	context, cancel := context.WithTimeout(context.Background(), contributionWait)
	r.cancel = cancel
	//go r.waitForContributions(proposal.Block.View())
	go func() {
		<-context.Done()
		if r.tree.IsRoot(r.opts.ID()) {
			r.logger.Debug("Context Completed ", r.ProposalMsg.Block.View())
			if r.aggregatedContribution.Participants().Len() == r.configuration.Len() {
				r.logger.Debug("Sending NewView ", r.ProposalMsg.Block.View())
				r.synchronizer.AdvanceView(hotstuff.NewSyncInfo().WithQC(
					hotstuff.NewQuorumCert(r.aggregatedContribution, r.ProposalMsg.Block.View(),
						r.ProposalMsg.Block.Hash())))
			} else if r.aggregatedContribution.Participants().Len() > 1 {
				r.logger.Debug("Performing second Chance ", r.ProposalMsg.Block.View())
				r.performSecondChance()
			}
		} else {
			pID, ok := r.tree.GetParent()
			if ok {
				r.SendContributionToNode(pID, r.aggregatedContribution)
				r.sendACKToSenders()
			}
		}
	}()
}

// OnACK verifies and stores the aggregated contribution
func (r *Iniva) OnACK(event ACKRecvEvent) {
	if r.ProposalMsg.Block.View() == hotstuff.View(event.contribution.View) {
		quorumSignature := hotstuffpb.QuorumSignatureFromProto(event.contribution.Signature)
		if r.verifyContribution(quorumSignature, r.ProposalMsg.Block.Hash()) {
			r.aggregatedContribution = quorumSignature
		}
	}
}

func (r *Iniva) sendACKToSenders() {
	if len(r.senders) != 0 && r.aggregatedContribution != nil {
		for _, nodeID := range r.senders {
			node, ok := r.nodes[nodeID]
			if !ok {
				r.logger.Error("node not found in map ", nodeID, r.nodes)
				continue
			}
			contribution := inivapb.Contribution{
				ID:        uint32(r.tree.ID),
				Signature: hotstuffpb.QuorumSignatureToProto(r.aggregatedContribution),
				View:      uint64(r.ProposalMsg.Block.View()),
			}
			r.logger.Debug("sending acknowledgement from ", r.opts.ID(), " to ",
				nodeID, " for view ", contribution.View)
			node.SendAcknowledgement(context.Background(), &contribution)
		}
	}
}

// SendContributionToNode sends the contribution to a node
func (r *Iniva) SendContributionToNode(nodeID hotstuff.ID, quorumSignature hotstuff.QuorumSignature) {
	emptyContribution := &inivapb.Contribution{}
	node, ok := r.nodes[nodeID]
	if !ok {
		r.logger.Error("node not found in map ", nodeID, r.nodes)
		return
	}
	if quorumSignature == nil {
		node.SendContribution(context.Background(), emptyContribution)
	} else {
		contribution := inivapb.Contribution{
			ID:        uint32(r.tree.ID),
			Signature: hotstuffpb.QuorumSignatureToProto(quorumSignature),
			View:      uint64(r.ProposalMsg.Block.View()),
		}
		r.logger.Debug("sending contribution from ", r.opts.ID(), " to ", nodeID, " for view ", contribution.View)
		node.SendContribution(context.Background(), &contribution)
	}
}

func (r *Iniva) reset() {
	r.beginDone = false
	r.aggregatedContribution = nil
	r.senders = make([]hotstuff.ID, 0)
	r.inSecondChance = false
}

func (r *Iniva) canMergeContributions(a, b hotstuff.QuorumSignature) bool {
	canMerge := true
	if a == nil || b == nil {
		r.logger.Info("one of it is nil")
		return false
	}
	a.Participants().RangeWhile(func(i hotstuff.ID) bool {
		b.Participants().RangeWhile(func(j hotstuff.ID) bool {
			// cannot merge a and b if they both contain a contribution from the same ID.
			if i == j {
				r.logger.Debug("one of it is same ", i)
				canMerge = false
			}
			return canMerge
		})
		return canMerge
	})
	return canMerge
}

func (r *Iniva) verifyContribution(signature hotstuff.QuorumSignature, hash hotstuff.Hash) bool {
	verified := false
	block, ok := r.blockChain.Get(hash)
	if !ok {
		return verified
	}
	verified = r.crypto.Verify(signature, block.ToBytes())
	return verified
}
func (r *Iniva) mergeWithContribution(currentSignature hotstuff.QuorumSignature) error {
	if currentSignature == nil {
		return errors.New("unable to verify the contribution")
	}
	isVerified := r.verifyContribution(currentSignature, r.ProposalMsg.Block.Hash())
	if !isVerified {
		r.logger.Info("Contribution verification failed for ", r.ProposalMsg.Block.View(),
			"from participants", currentSignature.Participants())
		return errors.New("unable to verify the contribution")
	}
	if r.aggregatedContribution == nil {
		r.aggregatedContribution = currentSignature
		return nil
	}

	//compiledSignature := hotstuffpb.QuorumSignatureFromProto(r.aggregatedContribution.Signature)
	if r.canMergeContributions(currentSignature, r.aggregatedContribution) {
		combined, err := r.crypto.Combine(currentSignature, r.aggregatedContribution)
		if err == nil {
			r.aggregatedContribution = combined
		} else {
			r.logger.Info("Failed to combine signatures: %v", err)
			return errors.New("unable to combine signature")
		}
	} else {
		r.logger.Debug("Failed to merge signatures due to overlap of signatures.")
		return errors.New("unable to merge signature")
	}
	return nil
}

type serviceImpl struct {
	r *Iniva
}

// SendAcknowledgement Handles incoming acknowledgements
func (i serviceImpl) SendAcknowledgement(_ gorums.ServerCtx, request *inivapb.Contribution) {
	i.r.logger.Debug("Received acknowledgment, storing the acknowledgement")
	i.r.eventLoop.AddEvent(ACKRecvEvent{contribution: request})
}

// Handles SecondChance requests
func (i serviceImpl) SecondChance(_ gorums.ServerCtx, proposal *hotstuffpb.Proposal) {
	i.r.logger.Debug("Received second chance proposal")
	proposeMsg := hotstuffpb.ProposalFromProto(proposal)
	i.r.eventLoop.AddEvent(proposeMsg)
}

// SendContribution handles the incoming contribution
func (i serviceImpl) SendContribution(_ gorums.ServerCtx, request *inivapb.Contribution) {

	i.r.logger.Debug("Received contribution for view ", request.View)
	i.r.eventLoop.AddEvent(ContributionRecvEvent{Contribution: request})

}

// ContributionRecvEvent is sent when a contribution is received.
type ContributionRecvEvent struct {
	Contribution *inivapb.Contribution
}

func (r *Iniva) randomizeIDs(hash hotstuff.Hash, leaderID hotstuff.ID) map[hotstuff.ID]int {
	//assign leader to the root of the tree.
	seed := r.opts.SharedRandomSeed() + int64(binary.LittleEndian.Uint64(hash[:]))
	totalNodes := r.configuration.Len()
	ids := make([]hotstuff.ID, 0, totalNodes)
	for id := range r.configuration.Replicas() {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// Shuffle the list of IDs using the shared random seed + the first 8 bytes of the hash.
	rnd := rand.New(rand.NewSource(seed))
	rnd.Shuffle(len(ids), reflect.Swapper(ids))
	lIndex := 0
	for index, id := range ids {
		if id == leaderID {
			lIndex = index
		}
	}
	currentRoot := ids[0]
	ids[0] = ids[lIndex]
	ids[lIndex] = currentRoot
	posMapping := make(map[hotstuff.ID]int)
	for index, ID := range ids {
		posMapping[ID] = index
	}
	return posMapping
}

// check if a is subset of b
func isSubSet(a, b []hotstuff.ID) bool {
	c := hotstuff.NewIDSet()
	for _, id := range b {
		c.Add(id)
	}
	for _, id := range a {
		if !c.Contains(id) {
			return false
		}
	}
	return true
}

// ACKRecvEvent is sent when ACK is received.
type ACKRecvEvent struct {
	contribution *inivapb.Contribution
}
