package sandboxmatrix

// tlsDirOverride, when non-nil, replaces the production tlsDir constant for
// tests. Set via withTLSDir; nil means "use tlsDir".
var tlsDirOverride *string
