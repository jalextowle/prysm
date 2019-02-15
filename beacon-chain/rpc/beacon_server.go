package rpc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/prysmaticlabs/prysm/shared/bytesutil"

	ptypes "github.com/gogo/protobuf/types"
	"github.com/prysmaticlabs/prysm/beacon-chain/db"
	pbp2p "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/rpc/v1"
	"github.com/prysmaticlabs/prysm/shared/params"
)

// BeaconServer defines a server implementation of the gRPC Beacon service,
// providing RPC endpoints for obtaining the canonical beacon chain head,
// fetching latest observed attestations, and more.
type BeaconServer struct {
	beaconDB            *db.BeaconDB
	ctx                 context.Context
	powChainService     powChainService
	chainService        chainService
	operationService    operationService
	incomingAttestation chan *pbp2p.Attestation
	canonicalStateChan  chan *pbp2p.BeaconState
	chainStartChan      chan time.Time
}

// WaitForChainStart queries the logs of the Deposit Contract in order to verify the beacon chain
// has started its runtime and validators begin their responsibilities. If it has not, it then
// subscribes to an event stream triggered by the powchain service whenever the ChainStart log does
// occur in the Deposit Contract on ETH 1.0.
func (bs *BeaconServer) WaitForChainStart(req *ptypes.Empty, stream pb.BeaconService_WaitForChainStartServer) error {
	ok, genesisTime, err := bs.powChainService.HasChainStartLogOccurred()
	if err != nil {
		return fmt.Errorf("could not determine if ChainStart log has occurred: %v", err)
	}
	if ok {
		res := &pb.ChainStartResponse{
			Started:     true,
			GenesisTime: genesisTime,
		}
		return stream.Send(res)
	}

	sub := bs.chainService.StateInitializedFeed().Subscribe(bs.chainStartChan)
	defer sub.Unsubscribe()
	for {
		select {
		case chainStartTime := <-bs.chainStartChan:
			log.Info("Sending ChainStart log and genesis time to connected validator clients")
			res := &pb.ChainStartResponse{
				Started:     true,
				GenesisTime: uint64(chainStartTime.Unix()),
			}
			return stream.Send(res)
		case <-sub.Err():
			log.Debug("Subscriber closed, exiting goroutine")
			return nil
		case <-bs.ctx.Done():
			log.Debug("RPC context closed, exiting goroutine")
			return nil
		}
	}
}

// CanonicalHead of the current beacon chain. This method is requested on-demand
// by a validator when it is their time to propose or attest.
func (bs *BeaconServer) CanonicalHead(ctx context.Context, req *ptypes.Empty) (*pbp2p.BeaconBlock, error) {
	block, err := bs.beaconDB.ChainHead()
	if err != nil {
		return nil, fmt.Errorf("could not get canonical head block: %v", err)
	}
	return block, nil
}

// LatestAttestation streams the latest processed attestations to the rpc clients.
func (bs *BeaconServer) LatestAttestation(req *ptypes.Empty, stream pb.BeaconService_LatestAttestationServer) error {
	sub := bs.operationService.IncomingAttFeed().Subscribe(bs.incomingAttestation)
	defer sub.Unsubscribe()
	for {
		select {
		case attestation := <-bs.incomingAttestation:
			log.Info("Sending attestation to RPC clients")
			if err := stream.Send(attestation); err != nil {
				return err
			}
		case <-sub.Err():
			log.Debug("Subscriber closed, exiting goroutine")
			return nil
		case <-bs.ctx.Done():
			log.Debug("RPC context closed, exiting goroutine")
			return nil
		}
	}
}

// Eth1Data is a mechanism used by block proposers vote on a recent Ethereum 1.0 block hash and an
// associated deposit root found in the Ethereum 1.0 deposit contract. When consensus is formed,
// state.latest_eth1_data is updated, and validator deposits up to this root can be processed.
// The deposit root can be calculated by calling the get_deposit_root() function of
// the deposit contract using the post-state of the block hash.
func (bs *BeaconServer) Eth1Data(ctx context.Context, _ *ptypes.Empty) (*pb.Eth1DataResponse, error) {
	beaconState, err := bs.beaconDB.State()
	if err != nil {
		return nil, fmt.Errorf("could not fetch beacon state: %v", err)
	}
	dataVoteObjects := []*pbp2p.Eth1DataVote{}
	for _, vote := range beaconState.Eth1DataVotes {
		eth1Hash := bytesutil.ToBytes32(vote.Eth1Data.BlockHash32)
		// Verify the block from the vote's block hash exists in the eth1.0 chain and fetch its height.
		blockExists, blockNumber, err := bs.powChainService.BlockExists(eth1Hash)
		if err != nil {
			log.Errorf("Could not verify block with hash exists in Eth1 chain: %#x: %v", eth1Hash, err)
			continue
		}
		// Fetch the current canonical chain height from the eth1.0 chain.
		currentHeight, err := bs.powChainService.CurrentHeight()
		if err != nil {
			log.Errorf("Could not fetch current Eth1 chain height: %v", err)
			continue
		}
		// Fetch the height of the block pointed to by the beacon state's latest_eth1_data.block_hash
		// in the canonical, eth1.0 chain.
		stateLatestEth1Hash := bytesutil.ToBytes32(beaconState.LatestEth1Data.BlockHash32)
		_, stateLatestEth1Height, err := bs.powChainService.BlockExists(stateLatestEth1Hash)
		if err != nil {
			log.Errorf("Could not verify block with hash exists in Eth1 chain: %#x: %v", eth1Hash, err)
			continue
		}
		// Let dataVoteObjects be the set of Eth1DataVote objects vote in state.eth1_data_votes where:
		// vote.eth1_data.block_hash is the hash of an eth1.0 block that is:
		//   (i) part of the canonical chain
		//   (ii) >= ETH1_FOLLOW_DISTANCE blocks behind the head
		//   (iii) newer than state.latest_eth1_data.block_data.
		// vote.eth1_data.deposit_root is the deposit root of the eth1.0 deposit contract
		// at the block defined by vote.eth1_data.block_hash.
		isBehindFollowDistance := blockNumber+params.BeaconConfig().Eth1FollowDistance < currentHeight
		isAheadStateLatestEth1Data := blockNumber > stateLatestEth1Height
		if blockExists && isBehindFollowDistance && isAheadStateLatestEth1Data {
			dataVoteObjects = append(dataVoteObjects, vote)
		}
	}

	// Now we handle the following two scenarios:
	// If dataVoteObjects is empty:
	// Let block_hash be the block hash of the ETH1_FOLLOW_DISTANCE'th ancestor of the head of
	// the canonical eth1.0 chain.
	// Let deposit_root be the deposit root of the eth1.0 deposit contract in the
	// post-state of the block referenced by block_hash.
	if len(dataVoteObjects) == 0 {
		// Fetch the current canonical chain height from the eth1.0 chain.
		currentHeight, err := bs.powChainService.CurrentHeight()
		if err != nil {
			return nil, fmt.Errorf("could not fetch current Eth1 chain height: %v", err)
		}
		blockHash, err := bs.powChainService.BlockHashByHeight(currentHeight - params.BeaconConfig().Eth1FollowDistance)
		if err != nil {
			return nil, fmt.Errorf("could not fetch ETH1_FOLLOW_DISTANCE ancestor: %v", err)
		}
		depositRoot := bs.powChainService.DepositRoot()
		return &pb.Eth1DataResponse{
			Eth1Data: &pbp2p.Eth1Data{
				DepositRootHash32: depositRoot[:],
				BlockHash32:       blockHash[:],
			},
		}, nil
	}

	// If dataVoteObjects is non-empty:
	// Let best_vote be the member of D that has the highest vote.eth1_data.vote_count,
	// breaking ties by favoring block hashes with higher associated block height.
	// Let block_hash = best_vote.eth1_data.block_hash.
	// Let deposit_root = best_vote.eth1_data.deposit_root.
	var bestVote *pbp2p.Eth1DataVote
	for _, vote := range dataVoteObjects {
		if vote.VoteCount > bestVote.VoteCount {
			bestVote = vote
		} else if vote.VoteCount == bestVote.VoteCount {
			bestVoteHash := bytesutil.ToBytes32(bestVote.Eth1Data.BlockHash32)
			voteHash := bytesutil.ToBytes32(vote.Eth1Data.BlockHash32)
			_, bestVoteHeight, err := bs.powChainService.BlockExists(bestVoteHash)
			if err != nil {
				log.Errorf("Could not fetch block height: %v", err)
				continue
			}
			_, voteHeight, err := bs.powChainService.BlockExists(voteHash)
			if err != nil {
				log.Errorf("Could not fetch block height: %v", err)
				continue
			}
			if voteHeight > bestVoteHeight {
				bestVote = vote
			}
		}
	}
	return &pb.Eth1DataResponse{
		Eth1Data: &pbp2p.Eth1Data{
			BlockHash32:       bestVote.Eth1Data.BlockHash32,
			DepositRootHash32: bestVote.Eth1Data.DepositRootHash32,
		},
	}, nil
}

// PendingDeposits returns a list of pending deposits that are ready for
// inclusion in the next beacon block.
func (bs *BeaconServer) PendingDeposits(ctx context.Context, _ *ptypes.Empty) (*pb.PendingDepositsResponse, error) {
	bNum := bs.powChainService.LatestBlockNumber()

	if bNum == nil {
		return nil, errors.New("latest PoW block number is unknown")
	}

	// Only request deposits that have passed the ETH1 follow distance window.
	bNum = bNum.Sub(bNum, big.NewInt(int64(params.BeaconConfig().Eth1FollowDistance)))

	return &pb.PendingDepositsResponse{PendingDeposits: bs.beaconDB.PendingDeposits(ctx, bNum)}, nil
}
