package cli

import (
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/processor"
)

func (s *sources) compiler() (*processor.Compiler, func(), error) {
	compiler := &processor.Compiler{}
	if len(s.cfg.Processors) == 0 {
		return compiler, compiler.Close, nil
	}
	if s.sess != nil {
		compiler.Cache = s.sess.cache
		return compiler, compiler.Close, nil
	}
	if s.cfg.CacheDir == "" {
		return compiler, compiler.Close, nil
	}
	compiled, err := cache.Open(s.cfg.CacheDir)
	if err != nil {
		return nil, nil, fault.Wrap(fault.Internal, err, "open compiler cache")
	}
	compiler.Cache = compiled
	return compiler, func() {
		compiler.Close()
		_ = compiled.Close()
	}, nil
}
