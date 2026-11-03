"""Starts the test BarPay (internal/cmd/barpay-test), the real BarPay with a fake PayPal, a fake LLM
and a fake clock, and calls it as the store app and the member's browser do.

The test BarPay listens on a free port of the loopback address and reports it once it accepts
connections, so tests running side by side never share a port. Closing its stdin stops it; it
also stops when this process ends and the pipe closes.
"""

import http.client
import itertools
import json
import select
import shutil
import subprocess
import tempfile
import urllib.parse
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import IO, Any

from helpers import SECRET

_REPO = Path(__file__).resolve().parents[2]

# The test BarPay's store API key: a fixed test value it has no setting for.
STORE_KEY = "test-store-key"

_START_TIMEOUT = 30.0
_STOP_TIMEOUT = 10.0
_CALL_TIMEOUT = 10.0


def build(out_dir: Path) -> Path:
    """Build the test BarPay from the Go module at the repository's root into out_dir."""
    go = shutil.which("go")
    if go is None:
        raise RuntimeError("Go is needed to build the test BarPay, and go is not on PATH")
    binary = out_dir / "barpay-test"
    subprocess.run(  # noqa: S603 -- the arguments are this file's own
        [go, "build", "-buildvcs=false", "-o", str(binary), "./internal/cmd/barpay-test"],
        cwd=_REPO,
        check=True,
    )
    return binary


class RealBarPay:
    """One running test BarPay."""

    def __init__(self, pid: int, ready: dict[str, Any]) -> None:
        self.pid = pid
        self.url: str = ready["url"]
        self.store_app_origin: str = ready["store_app_origin"]
        # How often the exit token changes, in seconds.
        self.exit_token_refresh: int = ready["exit_token_refresh_seconds"]
        # What it wrote besides the ready line, to stdout and stderr; set once it has stopped.
        self.output = b""
        address = urllib.parse.urlsplit(self.url)
        self._host = address.hostname or ""
        self._port = address.port
        self._keys = itertools.count()

    def store(self, method: str, path: str, body: Any = None, *, want: int = 200) -> Any:
        """The store app calling BarPay's store API; returns the decoded JSON answer, if any."""
        headers = {
            "Authorization": "Bearer " + STORE_KEY,
            "Idempotency-Key": f"key-{next(self._keys)}",
        }
        status, _, data = self._send(method, "/v1" + path, body, headers)
        # Only an error body is shown: another answer could hold an exit token.
        shown = data if status >= 400 else b""
        assert status == want, f"{method} {path}: {status} {shown!r}, want {want}"
        return json.loads(data) if data else None

    def browse(self, url: str) -> str:
        """The member's browser opening url, a BarPay URL, with no key; returns where it is sent."""
        if not url.startswith(self.url + "/"):
            raise ValueError(f"{url} is not the test BarPay's")
        status, location, _ = self._send("GET", url.removeprefix(self.url), None, {})
        assert status == 303, f"GET {url}: {status}"
        return location

    def linked_session(self, member_id: str) -> str:
        """Check member_id in, link PayPal through the member's browser and the fake PayPal, and
        report an order, so leaving charges the member. Returns the session's ID.
        """
        session: str = self.store("POST", "/checkins", {"member_id": member_id}, want=201)[
            "session_id"
        ]
        link = self.store(
            "POST",
            f"/members/{member_id}/paypal-link",
            {
                "state": "state-0123456789abcdef",
                "consent_version": "v1",
                "return_url": self.store_app_origin + "/settings",
            },
            want=201,
        )
        approve_url = self.browse(link["link_url"])
        approved = self._control(
            "POST", "/paypal/approve", {"approve_url": approve_url, "payer": "payer-" + member_id}
        )
        back = urllib.parse.urlsplit(self.browse(approved["return_url"]))
        code = urllib.parse.parse_qs(back.query)["code"][0]
        self.store("POST", f"/members/{member_id}/paypal-link/confirm", {"code": code}, want=201)
        order = {
            "order_id": "o-1",
            "lines": [
                {"name": "Beer", "quantity": 2, "unit_price": {"value": 650, "currency": "USD"}}
            ],
        }
        self.store("POST", f"/sessions/{session}/orders", order, want=201)
        return session

    def exit_token(self, session: str) -> str:
        """The session's current exit token, as the store app shows it in the exit QR."""
        token: str = self.store("GET", f"/sessions/{session}/exit-token")["token"]
        return token

    def advance(self, seconds: float) -> None:
        """Move the fake clock forward."""
        self._control("POST", "/clock/advance", {"seconds": seconds})

    def charges(self) -> int:
        """How many charges the fake PayPal made."""
        count: int = self._control("GET", "/paypal/charges", None)["count"]
        return count

    def _control(self, method: str, path: str, body: Any) -> Any:
        status, _, data = self._send(method, "/test" + path, body, {})
        shown = data if status >= 400 else b""
        assert status in (200, 204), f"{method} /test{path}: {status} {shown!r}"
        return json.loads(data) if data else None

    def _send(
        self, method: str, path: str, body: Any, headers: dict[str, str]
    ) -> tuple[int, str, bytes]:
        conn = http.client.HTTPConnection(self._host, self._port, timeout=_CALL_TIMEOUT)
        payload = None
        if body is not None:
            payload = json.dumps(body).encode()
            headers = headers | {"Content-Type": "application/json"}
        try:
            conn.request(method, path, body=payload, headers=headers)
            resp = conn.getresponse()
            return resp.status, resp.getheader("Location", ""), resp.read()
        finally:
            conn.close()


@contextmanager
def start(binary: Path, tmp: Path) -> Iterator[RealBarPay]:
    """Run the test BarPay until the block ends; it has stopped when start returns, even when the
    block raised. It keeps its store under tmp, so even a test BarPay that had to be killed leaves
    nothing outside it.
    """
    # Only the edge device's shared secret is given; the test BarPay reads nothing else.
    env = {"BARPAY_TEST_EDGE_SECRET": SECRET, "TMPDIR": str(tmp)}
    with tempfile.TemporaryFile() as stderr:
        proc = subprocess.Popen(  # noqa: S603 -- the arguments are this file's own
            [str(binary)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=stderr,
            env=env,
        )
        assert proc.stdin is not None
        assert proc.stdout is not None
        barpay: RealBarPay | None = None
        try:
            barpay = RealBarPay(proc.pid, _read_ready(proc.stdout, stderr))
            yield barpay
        finally:
            proc.stdin.close()
            try:
                proc.wait(timeout=_STOP_TIMEOUT)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            rest = proc.stdout.read()
            proc.stdout.close()
            stderr.seek(0)
            if barpay is not None:
                barpay.output = rest + stderr.read()


def _read_ready(stdout: IO[bytes], stderr: IO[bytes]) -> dict[str, Any]:
    readable, _, _ = select.select([stdout], [], [], _START_TIMEOUT)
    line = stdout.readline() if readable else b""
    if not line:
        stderr.seek(0)
        reason = stderr.read().decode(errors="replace")
        raise RuntimeError(f"the test BarPay did not start: {reason}")
    ready: dict[str, Any] = json.loads(line)
    return ready
