package mirror

// fileInUse: no lsof on Windows, and none is needed — Windows itself refuses
// to delete a file another process holds open, so GC's RemoveAll error path
// already skips in-use entries.
func fileInUse(string) bool { return false }
