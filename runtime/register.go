package runtime

// RegisterOptions controls which generated tools get registered with the
// server. Empty Enable / Tags means "no filter".
type RegisterOptions struct {
	Enable []string // package-name whitelist
	Tags   []string // tag whitelist
}

// Allow reports whether a tool from the given package with the given tags
// passes the configured filters.
func (o RegisterOptions) Allow(pkg string, tags []string) bool {
	if len(o.Enable) > 0 && !contains(o.Enable, pkg) {
		return false
	}
	if len(o.Tags) > 0 {
		for _, t := range tags {
			if contains(o.Tags, t) {
				return true
			}
		}
		return false
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
