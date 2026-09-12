// The end-to-end suite deliberately shares no code with the node: it talks to
// the cluster over HTTP the way any other client would, so a change that keeps
// the internals happy and breaks the contract still fails here. That also means
// no dependencies beyond the standard library, so it runs in a bare golang
// image with the module proxy switched off.
module rezoagwe/e2e

go 1.22
