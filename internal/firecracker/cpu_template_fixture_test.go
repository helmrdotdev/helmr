package firecracker

// customCPUTemplateSelector builds a validated custom CPU template selector for
// probe and contract fixtures.
func customCPUTemplateSelector(digest string) (CPUTemplateSelector, error) {
	selector := CPUTemplateSelector{Kind: CPUTemplateCustom, Digest: digest}
	if err := selector.Validate(); err != nil {
		return CPUTemplateSelector{}, err
	}
	return selector, nil
}
