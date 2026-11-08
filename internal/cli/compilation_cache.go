package cli

import (
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

func (s *sources) compiler(schemas map[string]runner.SchemaInfo) (*processor.Compiler, func(), error) {
	compiler := &processor.Compiler{VerifyOnly: s.cfg.Offline}
	typed, catalogSchema := false, false
	for _, definition := range s.cfg.Processors {
		if definition.Type != "custom" {
			typed = true
			break
		}
	}
	if !typed {
		return compiler, compiler.Close, nil
	}
	for _, schema := range schemas {
		if schema.Origin == originCatalog {
			catalogSchema = true
			break
		}
	}
	if !catalogSchema {
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
