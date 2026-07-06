package daemon

// ShQuote is exported for tests and the server's remote command construction.
func ShQuote(s string) string { return shQuote(s) }
