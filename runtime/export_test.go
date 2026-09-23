package node

// StopExecutorForTesting freezes the executor at an injected crash boundary.
// It deliberately leaves the store open for assertions about committed state.
func (node *Node) StopExecutorForTesting() {
	node.stop()
}
