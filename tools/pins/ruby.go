package pins

// The Ruby that builds the gem, the single pin of the release tooling
// (--ruby host and --ruby docker) and of the end-to-end suite (E41).
// RubyGems writes its own version into every gem, so the same pins give the
// same bytes on a runner with ruby/setup-ruby, which reads
// packaging/ruby/.ruby-version (kept equal to RubyVersion by a test), and in
// RubyImage.
const (
	RubyVersion     = "4.0.7"
	RubyGemsVersion = "4.0.20"
	RubyImage       = "ruby:" + RubyVersion + "-slim-trixie@sha256:db9ddd17cc6ac603f2497d98ac5c88e4118908d6f9a45f2422ebee141f91e485"
)
