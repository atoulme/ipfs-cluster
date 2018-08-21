package adder

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"strings"

	"github.com/ipfs/ipfs-cluster/adder/ipfsadd"
	"github.com/ipfs/ipfs-cluster/api"

	cid "github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-cmdkit/files"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log"
	merkledag "github.com/ipfs/go-merkledag"
	multihash "github.com/multiformats/go-multihash"
)

var logger = logging.Logger("adder")

// ClusterDAGService is an implementation of ipld.DAGService plus a Finalize
// method. ClusterDAGServices can be used to provide Adders with a different
// add implementation.
type ClusterDAGService interface {
	ipld.DAGService
	// Finalize receives the IPFS content root CID as
	// returned by the ipfs adder.
	Finalize(ctx context.Context, ipfsRoot *cid.Cid) (*cid.Cid, error)
}

// Adder is used to add content to IPFS Cluster using an implementation of
// ClusterDAGService.
type Adder struct {
	ctx    context.Context
	cancel context.CancelFunc

	dgs ClusterDAGService

	params *api.AddParams

	// AddedOutput updates are placed on this channel
	// whenever a block is processed. They contain information
	// about the block, the CID, the Name etc. and are mostly
	// meant to be streamed back to the user.
	output chan *api.AddedOutput
}

// New returns a new Adder with the given ClusterDAGService, add options and a
// channel to send updates during the adding process.
//
// An Adder may only be used once.
func New(ds ClusterDAGService, p *api.AddParams, out chan *api.AddedOutput) *Adder {
	// Discard all progress update output as the caller has not provided
	// a channel for them to listen on.
	if out == nil {
		out = make(chan *api.AddedOutput, 100)
		go func() {
			for range out {
			}
		}()
	}

	return &Adder{
		dgs:    ds,
		params: p,
		output: out,
	}
}

func (a *Adder) setContext(ctx context.Context) {
	if a.ctx == nil { // only allows first context
		ctxc, cancel := context.WithCancel(ctx)
		a.ctx = ctxc
		a.cancel = cancel
	}
}

// FromMultipart adds content from a multipart.Reader. The adder will
// no longer be usable after calling this method.
func (a *Adder) FromMultipart(ctx context.Context, r *multipart.Reader) (*cid.Cid, error) {
	logger.Debugf("adding from multipart with params: %+v", a.params)

	f := &files.MultipartFile{
		Mediatype: "multipart/form-data",
		Reader:    r,
	}
	defer f.Close()
	return a.FromFiles(ctx, f)
}

// FromFiles adds content from a files.File. The adder will no longer
// be usable after calling this method.
func (a *Adder) FromFiles(ctx context.Context, f files.File) (*cid.Cid, error) {
	logger.Debugf("adding from files")
	a.setContext(ctx)

	if a.ctx.Err() != nil { // don't allow running twice
		return nil, a.ctx.Err()
	}

	defer a.cancel()
	defer close(a.output)

	ipfsAdder, err := ipfsadd.NewAdder(a.ctx, a.dgs)
	if err != nil {
		logger.Error(err)
		return nil, err
	}

	ipfsAdder.Hidden = a.params.Hidden
	ipfsAdder.Trickle = a.params.Layout == "trickle"
	ipfsAdder.RawLeaves = a.params.RawLeaves
	ipfsAdder.Wrap = a.params.Wrap
	ipfsAdder.Chunker = a.params.Chunker
	ipfsAdder.Out = a.output
	ipfsAdder.Progress = a.params.Progress

	// Set up prefix
	prefix, err := merkledag.PrefixForCidVersion(a.params.CidVersion)
	if err != nil {
		return nil, fmt.Errorf("bad CID Version: %s", err)
	}

	hashFunCode, ok := multihash.Names[strings.ToLower(a.params.HashFun)]
	if !ok {
		return nil, fmt.Errorf("unrecognized hash function: %s", a.params.HashFun)
	}
	prefix.MhType = hashFunCode
	prefix.MhLength = -1
	ipfsAdder.CidBuilder = &prefix

	for {
		select {
		case <-a.ctx.Done():
			return nil, a.ctx.Err()
		default:
			err := addFile(f, ipfsAdder)
			if err == io.EOF {
				goto FINALIZE
			}
			if err != nil {
				logger.Error("error adding to cluster: ", err)
				return nil, err
			}
		}
	}

FINALIZE:
	adderRoot, err := ipfsAdder.Finalize()
	if err != nil {
		return nil, err
	}
	clusterRoot, err := a.dgs.Finalize(a.ctx, adderRoot.Cid())
	if err != nil {
		logger.Error("error finalizing adder:", err)
		return nil, err
	}
	logger.Infof("%s successfully added to cluster", clusterRoot)
	return clusterRoot, nil
}

func addFile(fs files.File, ipfsAdder *ipfsadd.Adder) error {
	f, err := fs.NextFile()
	if err != nil {
		return err
	}
	defer f.Close()

	logger.Debugf("ipfsAdder AddFile(%s)", f.FullPath())
	return ipfsAdder.AddFile(f)
}
