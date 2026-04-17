package router

func (r *Router) nativeNamespaceGeneration() uint64 {
	if generation := r.namespaceGeneration.Load(); generation != 0 {
		return generation
	}
	return 1
}

func (r *Router) nativeEffectiveGeneration() uint64 {
	return (uint64(r.version.Load()) << 32) | (r.nativeNamespaceGeneration() & 0xffffffff)
}

func (r *Router) bumpNativeNamespaceGeneration(reason string) uint64 {
	next := r.namespaceGeneration.Add(1)
	if reason != "" {
		r.logger.Debug("bumped native namespace generation",
			"generation", next,
			"reason", reason)
	}
	return next
}
