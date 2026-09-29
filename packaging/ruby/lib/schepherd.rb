# frozen_string_literal: true

require "rbconfig"

# Schepherd delivers pinned JSON Schemas from an OCI catalog to verified local
# files. This gem ships the native schepherd binary of every supported
# platform under libexec/; the schepherd executable runs the one built for the
# running system.
module Schepherd
  # Names the environment variable that holds the absolute path of a
  # schepherd binary to run instead of the bundled one, for systems without a
  # prebuilt binary.
  BINARY_OVERRIDE = "SCHEPHERD_BINARY"

  # The os-cpu pairs of libexec/schepherd-<os>-<cpu>. The same table exists in
  # tools/release/internal/wrappers, which packs the gem; a Go test keeps the two
  # identical.
  TARGETS = [
    %w[darwin arm64],
    %w[darwin x64],
    %w[linux arm64],
    %w[linux x64],
    %w[windows arm64],
    %w[windows x64],
  ].freeze

  # Raised when there is no binary to run.
  class Error < StandardError; end

  module_function

  # Returns the absolute path of the binary to run: the SCHEPHERD_BINARY
  # override, or the bundled binary of the platform.
  def executable(env: ENV, platform: Gem::Platform.local, host: RbConfig::CONFIG,
                 root: File.expand_path("..", __dir__))
    override = binary_override(env)
    return override if override

    pair = target(platform, host)
    raise Error, unsupported_message(platform) unless TARGETS.include?(pair)

    os, cpu = pair
    path = File.join(root, "libexec", "schepherd-#{os}-#{cpu}", os == "windows" ? "schepherd.exe" : "schepherd")
    return path if File.file?(path)

    raise Error, "schepherd: the binary #{path} for #{os}-#{cpu} is missing from the gem. " \
                 "Reinstall it with `gem install schepherd`, or set #{BINARY_OVERRIDE} to the absolute path " \
                 "of a schepherd binary."
  end

  # Returns the [os, cpu] pair of the bundled binary for a platform, or nil.
  # JRuby reports the platform universal-java, so the host OS and CPU of
  # RbConfig answer what Gem::Platform cannot.
  def target(platform = Gem::Platform.local, host = RbConfig::CONFIG)
    os = os_name(platform.os) || os_name(host["host_os"])
    cpu = cpu_name(platform.cpu) || cpu_name(host["host_cpu"])
    [os, cpu] if os && cpu
  end

  def os_name(os)
    case os.to_s
    when /\Alinux/ then "linux"
    when /\Adarwin/ then "darwin"
    when /\A(?:mingw|mswin|windows)/ then "windows"
    end
  end

  # Apple silicon Rubies may report arm64e, and the system Ruby of macOS a
  # universal.<cpu> CPU.
  def cpu_name(cpu)
    case cpu.to_s.sub(/\Auniversal\./, "")
    when /\Aarm64/, "aarch64" then "arm64"
    when "x86_64", "x64", "amd64" then "x64"
    end
  end

  # Returns the validated SCHEPHERD_BINARY path, or nil when it is unset or
  # empty.
  def binary_override(env = ENV)
    value = env[BINARY_OVERRIDE]
    return nil if value.nil? || value.empty?

    unless File.absolute_path?(value)
      raise Error, "schepherd: #{BINARY_OVERRIDE} must be an absolute path to the schepherd binary, got #{value.inspect}"
    end
    return value if File.file?(value)

    raise Error, "schepherd: #{BINARY_OVERRIDE} points to #{value.inspect}, which is not a regular file"
  end

  def unsupported_message(platform)
    supported = TARGETS.map { |os, cpu| "#{os}-#{cpu}" }.join(", ")
    "schepherd: no prebuilt binary for #{platform}. Supported platforms: #{supported}. " \
      "Set #{BINARY_OVERRIDE} to the absolute path of a schepherd binary built for this system."
  end
  private_class_method :unsupported_message
end
