"""Tags the wheel for the platform of the one binary staged in schepherd/bin."""

import os

from hatchling.builders.hooks.plugin.interface import BuildHookInterface


class PlatformWheelHook(BuildHookInterface):
  def initialize(self, version, build_data):
    platform = os.environ.get("SCHEPHERD_WHEEL_PLATFORM", "")
    if not platform:
      raise RuntimeError(
        "SCHEPHERD_WHEEL_PLATFORM is not set; "
        "build the wheels with `go run ./tools/release packages build`"
      )
    build_data["tag"] = "py3-none-" + platform
    build_data["pure_python"] = False
