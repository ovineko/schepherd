"""Runs the native schepherd binary that this wheel ships for its platform."""

from __future__ import annotations

import os
import signal
import subprocess
import sys
from typing import NoReturn

ISSUES_URL = "https://github.com/ovineko/schepherd/issues"

# CPython sets these signals to SIG_IGN at startup, and an ignored signal
# stays ignored across exec. subprocess resets the same list in its children
# (restore_signals), so the binary starts as if a shell had started it.
PYTHON_IGNORED_SIGNALS = ("SIGPIPE", "SIGXFZ", "SIGXFSZ")

# The console delivers these to every process attached to it, the binary
# included, which shuts down by itself; the launcher only waits for it.
CONSOLE_SIGNALS = ("SIGINT", "SIGBREAK")


def binary_path() -> str:
  name = "schepherd.exe" if sys.platform == "win32" else "schepherd"
  return os.path.join(os.path.dirname(os.path.abspath(__file__)), "bin", name)


def main() -> int:
  binary = binary_path()
  argv = [binary, *sys.argv[1:]]
  try:
    if sys.platform == "win32":
      return wait_for(argv)
    replace_process(binary, argv)
  except OSError as error:
    sys.stderr.write(
      f"schepherd: cannot start {binary}: {error.strerror or error}\n"
      "Reinstall the schepherd wheel for this platform, "
      f"or report the problem at {ISSUES_URL}\n"
    )
  return 1


def replace_process(binary: str, argv: list[str]) -> NoReturn:
  for name in PYTHON_IGNORED_SIGNALS:
    signum = getattr(signal, name, None)
    if signum is not None:
      signal.signal(signum, signal.SIG_DFL)
  # The launcher runs its own bundled binary with the user's arguments, as
  # the user asked; no shell is involved.
  # bearer:disable python_lang_code_injection
  os.execv(binary, argv)


def wait_for(argv: list[str]) -> int:
  # Windows has no exec: os.execv would start a new process and end this
  # one at once, so the console would not wait for the binary.
  for name in CONSOLE_SIGNALS:
    signal.signal(getattr(signal, name), signal.SIG_IGN)
  # bearer:disable python_lang_os_command_injection
  code = subprocess.call(argv)
  # Exit statuses are unsigned 32-bit on Windows, and older CPython versions
  # fail on sys.exit values above 2**31 - 1 (gh-125842). The signed form
  # reaches ExitProcess unchanged, such as 0xC000013A after Ctrl+C.
  return code - 2**32 if code >= 2**31 else code
