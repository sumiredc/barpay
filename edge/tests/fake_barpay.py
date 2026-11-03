"""A fake BarPay that answers the edge device API as docs/api/edge.yaml defines it.

It checks the shared secret and the shape of each request as BarPay does, and otherwise gives
the answers a test queues for an operation. Every answer it gives is kept, and the ones not
marked off_spec are checked against edge.yaml when the test ends (conftest.py).
"""

import contextlib
import json
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from spec import Spec


@dataclass(frozen=True)
class Answer:
    status: int
    body: bytes = b""
    # An answer edge.yaml does not define, such as a 5xx, for a test of how the client takes it.
    off_spec: bool = False
    # Seconds to wait before answering.
    delay: float = 0.0
    # Close the connection without answering.
    hang_up: bool = False
    headers: dict[str, str] = field(default_factory=dict)


def answer(status: int, body: Any = None) -> Answer:
    """An answer edge.yaml defines, with body sent as JSON."""
    return Answer(status, b"" if body is None else json.dumps(body).encode())


def error(status: int, code: str, message: str = "for developers") -> Answer:
    return answer(status, {"error": {"code": code, "message": message}})


def off_spec(status: int, body: bytes = b"", **kwargs: Any) -> Answer:
    return Answer(status, body, off_spec=True, **kwargs)


@dataclass(frozen=True)
class Request:
    method: str
    path: str
    # Out of the repr, so a failing assertion on requests does not print the shared secret.
    headers: dict[str, str] = field(repr=False)
    body: bytes = b""


@dataclass(frozen=True)
class Given:
    """An answer the fake gave, and to which operation (None when the path is not edge.yaml's)."""

    operation: str | None
    answer: Answer


class FakeBarPay:
    def __init__(self, spec: Spec, secret: str, prefix: str = "") -> None:
        self.spec = spec
        self.secret = secret
        self.prefix = prefix
        self.requests: list[Request] = []
        self.given: list[Given] = []
        self._queued: dict[str, deque[Answer]] = {}
        # The unlock IDs given in answers, which reportUnlockResult takes.
        self._unlocks: set[str] = set()
        self._mu = threading.Lock()
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), self._handler())
        self._server.daemon_threads = True
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
        )

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host!s}:{port}{self.prefix}"

    def start(self) -> None:
        self._thread.start()

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    def queue(self, operation: str, *answers: Answer) -> None:
        """Answer the next calls of operation with answers, after checking the secret and the
        request as BarPay does.
        """
        with self._mu:
            self._queued.setdefault(operation, deque()).extend(answers)

    def _respond(self, method: str, path: str, headers: dict[str, str], body: bytes) -> Given:
        with self._mu:
            self.requests.append(Request(method, path, headers, body))
        if not path.startswith(self.prefix + self.spec.prefix + "/"):
            return Given(None, off_spec(404))
        found = self.spec.find(method, path.removeprefix(self.prefix + self.spec.prefix))
        if found is None:
            return Given(None, off_spec(404))
        op, params = found
        with self._mu:
            queued = self._queued.get(op.id)
            if queued and queued[0].off_spec:
                return Given(op.id, queued.popleft())
        if headers.get("authorization") != "Bearer " + self.secret:
            return Given(op.id, error(401, "unauthorized"))
        try:
            decoded = json.loads(body) if body else None
        except ValueError:
            return Given(op.id, error(400, "invalid_request"))
        if self.spec.request_errors(op, params, decoded, bool(body)):
            return Given(op.id, error(400, "invalid_request"))
        with self._mu:
            if queued:
                given = queued.popleft()
            elif op.id == "reportUnlockResult" and params["unlock_id"] not in self._unlocks:
                given = error(404, "unlock_not_found")
            else:
                given = self._default(op.id)
            if op.id in ("scanExitToken", "waitForUnlock") and given.status == 200:
                unlock_id = json.loads(given.body).get("unlock_id")
                if unlock_id:
                    self._unlocks.add(unlock_id)
        return Given(op.id, given)

    @staticmethod
    def _default(operation: str) -> Answer:
        return {
            "scanExitToken": answer(
                200, {"unlock": False, "reader_message": "Please show a new QR code"}
            ),
            "waitForUnlock": answer(204),
            "reportUnlockResult": answer(204),
            "sendSignals": answer(204),
        }[operation]

    def _handler(self) -> type[BaseHTTPRequestHandler]:
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def _serve(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length)
                headers = {k.lower(): v for k, v in self.headers.items()}
                given = fake._respond(self.command, self.path, headers, body)
                with fake._mu:
                    fake.given.append(given)
                a = given.answer
                time.sleep(a.delay)
                if a.hang_up:
                    self.close_connection = True
                    return
                self.send_response(a.status)
                for k, v in a.headers.items():
                    self.send_header(k, v)
                if a.body:
                    self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(a.body)))
                self.end_headers()
                # The client stops reading an answer that is too large and closes the connection.
                with contextlib.suppress(BrokenPipeError, ConnectionResetError):
                    self.wfile.write(a.body)

            do_GET = do_POST = do_PUT = do_DELETE = _serve

            def log_message(self, format: str, *args: Any) -> None:
                pass

        return Handler
