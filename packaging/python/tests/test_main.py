import io
import os
import signal
import subprocess
import sys
import unittest
from unittest import mock

from schepherd import main as launcher

PACKAGE_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def run_launcher(binary, args, stdin=b""):
  """Runs the launcher in a new interpreter with binary_path() replaced."""
  script = (
    "import sys\n"
    "from schepherd import main as launcher\n"
    f"launcher.binary_path = lambda: {binary!r}\n"
    f"sys.argv = ['schepherd'] + {args!r}\n"
    "sys.exit(launcher.main())\n"
  )
  env = dict(os.environ, PYTHONPATH=PACKAGE_ROOT, PYTHONDONTWRITEBYTECODE="1")
  return subprocess.run(
    [sys.executable, "-c", script],
    input=stdin,
    capture_output=True,
    env=env,
    check=False,
  )


class BinaryPathTest(unittest.TestCase):
  def test_binary_lies_in_the_package(self):
    package = os.path.dirname(os.path.abspath(launcher.__file__))
    with mock.patch.object(launcher.sys, "platform", "linux"):
      self.assertEqual(launcher.binary_path(), os.path.join(package, "bin", "schepherd"))
    with mock.patch.object(launcher.sys, "platform", "win32"):
      self.assertEqual(launcher.binary_path(), os.path.join(package, "bin", "schepherd.exe"))


class MainTest(unittest.TestCase):
  def test_posix_replaces_the_process_with_the_arguments(self):
    with mock.patch.object(launcher.sys, "platform", "linux"), mock.patch.object(
      launcher.sys, "argv", ["schepherd", "version", "--json"]
    ), mock.patch.object(launcher, "replace_process") as replace:
      launcher.main()
      binary = launcher.binary_path()
    replace.assert_called_once_with(binary, [binary, "version", "--json"])

  def test_windows_returns_the_exit_status(self):
    with mock.patch.object(launcher.sys, "platform", "win32"), mock.patch.object(
      launcher.sys, "argv", ["schepherd", "list"]
    ), mock.patch.object(launcher, "wait_for", return_value=6) as wait:
      self.assertEqual(launcher.main(), 6)
      binary = launcher.binary_path()
    wait.assert_called_once_with([binary, "list"])

  def test_start_failure_is_reported(self):
    stderr = io.StringIO()
    error = FileNotFoundError(2, "No such file or directory")
    with mock.patch.object(launcher.sys, "platform", "linux"), mock.patch.object(
      launcher, "replace_process", side_effect=error
    ), mock.patch.object(launcher.sys, "stderr", stderr):
      self.assertEqual(launcher.main(), 1)
    self.assertIn("cannot start", stderr.getvalue())
    self.assertIn("No such file or directory", stderr.getvalue())


class ReplaceProcessTest(unittest.TestCase):
  def test_restores_the_signals_python_ignores(self):
    with mock.patch.object(launcher.os, "execv") as execv, mock.patch.object(
      launcher.signal, "signal"
    ) as set_signal:
      launcher.replace_process("/opt/schepherd", ["/opt/schepherd", "list"])
    execv.assert_called_once_with("/opt/schepherd", ["/opt/schepherd", "list"])
    restored = [c.args[0] for c in set_signal.call_args_list if c.args[1] == signal.SIG_DFL]
    expected = [getattr(signal, n) for n in launcher.PYTHON_IGNORED_SIGNALS if hasattr(signal, n)]
    self.assertEqual(restored, expected)


class WaitForTest(unittest.TestCase):
  def wait_for(self, status):
    with mock.patch.object(launcher.signal, "SIGBREAK", 21, create=True), mock.patch.object(
      launcher.signal, "signal"
    ) as set_signal, mock.patch.object(launcher.subprocess, "call", return_value=status) as call:
      code = launcher.wait_for(["schepherd.exe", "list"])
    call.assert_called_once_with(["schepherd.exe", "list"])
    ignored = [c.args[0] for c in set_signal.call_args_list if c.args[1] == signal.SIG_IGN]
    self.assertEqual(ignored, [signal.SIGINT, 21])
    return code

  def test_exit_status_passes_through(self):
    self.assertEqual(self.wait_for(0), 0)
    self.assertEqual(self.wait_for(6), 6)

  def test_ntstatus_keeps_its_bits(self):
    code = self.wait_for(0xC000013A)
    self.assertEqual(code, -1073741510)
    self.assertEqual(code & 0xFFFFFFFF, 0xC000013A)


class ChildProcessTest(unittest.TestCase):
  """Runs a real child: exec on POSIX, subprocess.call on Windows."""

  def test_arguments_standard_streams_and_exit_status(self):
    child = "import sys; data = sys.stdin.read(); print('got ' + data + ' ' + sys.argv[1]); sys.exit(int(data))"
    result = run_launcher(sys.executable, ["-c", child, "arg with spaces"], stdin=b"7")
    self.assertEqual(result.returncode, 7, result.stderr)
    self.assertEqual(result.stdout.decode().strip(), "got 7 arg with spaces")

  def test_missing_binary(self):
    result = run_launcher(os.path.join(PACKAGE_ROOT, "no-such-binary"), ["version"])
    self.assertEqual(result.returncode, 1)
    self.assertIn(b"cannot start", result.stderr)


if __name__ == "__main__":
  unittest.main()
