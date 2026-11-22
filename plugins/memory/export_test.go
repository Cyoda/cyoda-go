package memory

// CommittedLogLenForTest returns the length of f's committed log. The SQLite
// plugin exports the same helper, so the two backends' task tests share one
// body.
func CommittedLogLenForTest(f *StoreFactory) int {
	return f.txManager.CommittedLogLen()
}
