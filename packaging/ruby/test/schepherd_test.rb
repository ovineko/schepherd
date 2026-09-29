# frozen_string_literal: true

require "minitest/autorun"
require "fileutils"
require "open3"
require "rbconfig"
require "tmpdir"
require_relative "../lib/schepherd"

class SchepherdTest < Minitest::Test
  WRAPPER = File.expand_path("../bin/schepherd", __dir__)
  NO_HOST = {}.freeze

  # [cpu, os, version] as Gem::Platform parses RUBY_PLATFORM, which older
  # RubyGems parse differently for platforms that did not exist yet, such as
  # x64-mingw-ucrt.
  PLATFORMS = {
    ["x86_64", "linux", nil] => %w[linux x64],
    ["x86_64", "linux", "musl"] => %w[linux x64],
    ["aarch64", "linux", nil] => %w[linux arm64],
    ["aarch64", "linux", "gnu"] => %w[linux arm64],
    ["arm64", "darwin", "23"] => %w[darwin arm64],
    ["arm64e", "darwin", "24"] => %w[darwin arm64],
    ["universal.arm64e", "darwin", "22"] => %w[darwin arm64],
    ["universal.x86_64", "darwin", "21"] => %w[darwin x64],
    ["x86_64", "darwin", "22"] => %w[darwin x64],
    ["x64", "mingw", "ucrt"] => %w[windows x64],
    ["x64", "mingw32", nil] => %w[windows x64],
    ["aarch64", "mingw", "ucrt"] => %w[windows arm64],
    ["x64", "mswin64", "140"] => %w[windows x64],
  }.freeze

  UNSUPPORTED = [
    ["x86_64", "freebsd", "14"],
    ["x86", "linux", nil],
    ["powerpc64le", "linux", nil],
    ["aarch64_be", "linux", nil],
    ["x86", "mingw32", nil],
    ["x64", "unknown", nil],
  ].freeze

  def test_maps_every_platform_to_its_binary
    PLATFORMS.each do |parts, want|
      assert_equal want, Schepherd.target(Gem::Platform.new(parts), NO_HOST), parts.inspect
    end
  end

  def test_every_target_is_reachable
    assert_equal Schepherd::TARGETS.sort, PLATFORMS.values.uniq.sort
  end

  def test_refuses_platforms_without_a_binary
    UNSUPPORTED.each do |parts|
      refute Schepherd::TARGETS.include?(Schepherd.target(Gem::Platform.new(parts), NO_HOST)), parts.inspect
    end
  end

  def test_falls_back_to_the_host_for_jruby
    java = Gem::Platform.new(["universal", "java", "21"])

    assert_equal %w[linux x64], Schepherd.target(java, "host_os" => "linux", "host_cpu" => "x86_64")
    assert_equal %w[darwin arm64], Schepherd.target(java, "host_os" => "darwin23", "host_cpu" => "arm64")
    assert_equal %w[windows x64], Schepherd.target(java, "host_os" => "mswin32", "host_cpu" => "x86_64")
    assert_nil Schepherd.target(java, NO_HOST)
  end

  def test_finds_the_bundled_binary
    Dir.mktmpdir do |root|
      linux = bundle(root, "linux", "x64", "schepherd")
      windows = bundle(root, "windows", "arm64", "schepherd.exe")

      assert_equal linux, Schepherd.executable(env: {}, platform: Gem::Platform.new(["x86_64", "linux", nil]),
                                                        host: NO_HOST, root: root)
      assert_equal windows, Schepherd.executable(env: {}, platform: Gem::Platform.new(["aarch64", "mingw", "ucrt"]),
                                                          host: NO_HOST, root: root)
    end
  end

  def test_reports_a_missing_binary
    Dir.mktmpdir do |root|
      error = assert_raises(Schepherd::Error) do
        Schepherd.executable(env: {}, platform: Gem::Platform.new(["arm64", "darwin", "23"]), host: NO_HOST, root: root)
      end

      assert_includes error.message, File.join("libexec", "schepherd-darwin-arm64", "schepherd")
    end
  end

  def test_reports_an_unsupported_platform
    error = assert_raises(Schepherd::Error) do
      Schepherd.executable(env: {}, platform: Gem::Platform.new(["x86_64", "freebsd", "14"]), host: NO_HOST, root: Dir.pwd)
    end

    assert_includes error.message, "x86_64-freebsd-14"
    assert_includes error.message, "linux-x64"
    assert_includes error.message, Schepherd::BINARY_OVERRIDE
  end

  def test_override_replaces_the_bundled_binary
    Dir.mktmpdir do |dir|
      binary = File.join(dir, "custom")
      File.write(binary, "")

      platform = Gem::Platform.new(["x86_64", "freebsd", "14"])
      assert_equal binary, Schepherd.executable(env: { "SCHEPHERD_BINARY" => binary }, platform: platform, host: NO_HOST)
      assert_nil Schepherd.binary_override("SCHEPHERD_BINARY" => "")

      ["relative/schepherd", File.join(dir, "missing"), dir].each do |value|
        assert_raises(Schepherd::Error, value) { Schepherd.binary_override("SCHEPHERD_BINARY" => value) }
      end
    end
  end

  def test_wrapper_passes_arguments_stdin_and_exit_status
    input = "line 1\nline 2\né\x00\xff".b
    args = ["a b", "", "c\"d", "ü", "--flag=x y"]
    probe = "STDIN.binmode; STDOUT.binmode; STDOUT.write(STDIN.read); STDOUT.write(ARGV.join('|')); exit 7"

    out, err, status = Open3.capture3({ "SCHEPHERD_BINARY" => RbConfig.ruby }, RbConfig.ruby, WRAPPER, "-e", probe,
                                      *args, stdin_data: input, binmode: true)

    assert_equal 7, status.exitstatus, err
    assert_equal input + args.join("|").b, out
  end

  def test_wrapper_reports_errors_with_status_1
    _, err, status = Open3.capture3({ "SCHEPHERD_BINARY" => "relative" }, RbConfig.ruby, WRAPPER, "version")

    assert_equal 1, status.exitstatus
    assert_includes err, "SCHEPHERD_BINARY must be an absolute path"
  end

  unless Gem.win_platform?
    def test_wrapper_never_uses_a_shell
      Dir.mktmpdir do |dir|
        binary = File.join(dir, "ru by;touch injected")
        File.symlink(RbConfig.ruby, binary)

        out, err, status = Open3.capture3({ "SCHEPHERD_BINARY" => binary }, RbConfig.ruby, WRAPPER,
                                          stdin_data: "print :ok", chdir: dir)

        assert status.success?, err
        assert_equal "ok", out
        refute File.exist?(File.join(dir, "injected"))
      end
    end
  end

  private

  def bundle(root, os, cpu, name)
    dir = File.join(root, "libexec", "schepherd-#{os}-#{cpu}")
    FileUtils.mkdir_p(dir)
    path = File.join(dir, name)
    File.write(path, "")
    path
  end
end
