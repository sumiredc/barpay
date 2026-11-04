"""Calls BarPay's edge device API (docs/api/edge.yaml) with the edge device's shared secret.

Only the four operations of edge.yaml, and only under /v1/edge. A failure is either Refused or
NoAnswer, so the caller can decide whether to send again.
"""

import http.client
import ipaddress
import json
import math
import re
import urllib.parse
from dataclasses import dataclass
from enum import StrEnum
from typing import Any, TypeIs

# How long waitForUnlock waits at most before it answers (edge.yaml), in seconds.
UNLOCK_WAIT = 25.0

# Every answer of edge.yaml is far smaller; reading stops here so a wrong server cannot fill memory.
_MAX_RESPONSE_SIZE = 1 << 20

_UNLOCK_ID = re.compile(r"glk_[A-Za-z0-9]{16,32}")

# Visible ASCII: anything else could not be sent in the Authorization header as it is, and
# http.client would quote the value in the error it raises.
_SECRET = re.compile(r"[\x21-\x7e]+")


# Why a URL is refused by secure_url, worded as BarPay's own settings word it. It never quotes
# the URL, which can carry credentials in its user info or query.
INSECURE_URL = (
    "must use https; http is accepted only to a loopback IP address such as 127.0.0.1 or "
    "[::1], not to a name such as localhost"
)


def secure_url(url: str) -> bool:
    """Whether the shared secret may be sent to url: https, or http to a loopback IP address
    written as one (127.0.0.0/8 as IPv4, or ::1), as BarPay's rule for its own URLs (the PRD's
    UC8). A name such as localhost is refused even over http, as what it resolves to is not
    known when the setting is read, and so is ::1 with a zone or 127.0.0.1 within IPv6.
    """
    try:
        parts = urllib.parse.urlsplit(url)
        host = parts.hostname
    except ValueError:
        return False
    if parts.scheme == "https":
        return True
    if parts.scheme != "http" or not host:
        return False
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return False
    if isinstance(ip, ipaddress.IPv4Address):
        return ip.is_loopback
    # ::1 with a zone, such as ::1%lo, is not equal to ::1.
    return ip == ipaddress.IPv6Address("::1")


class ReaderMessage(StrEnum):
    """edge.yaml's ScanResult.reader_message."""

    THANK_YOU = "Thank you"
    PAY_AT_REGISTER = "Please pay at the register"
    SEE_STAFF = "Please see staff"
    SHOW_NEW_QR = "Please show a new QR code"


@dataclass(frozen=True)
class ScanResult:
    """edge.yaml's ScanResult. unlock_id is set exactly when unlock is True."""

    unlock: bool
    unlock_id: str | None
    reader_message: ReaderMessage


class BarPayError(Exception):
    """A call that did not succeed. Its text never holds the shared secret."""


class Refused(BarPayError):
    """BarPay answered with an error edge.yaml defines for the operation (400, 401, 404).

    BarPay received the request and kept nothing, so sending it again gets the same answer.
    """

    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(f"BarPay refused with {status} {code}: {message}")
        self.status = status
        self.code = code


class NoAnswer(BarPayError):
    """It is not known whether BarPay received the request or what it did.

    BarPay was not reached, did not answer in time, or answered something edge.yaml does not
    define for the operation, such as a 5xx or a body that is not the operation's.
    """


class Client:
    """Safe to use from several threads: each call opens its own connection.

    Each call's timeout bounds connecting and each read from BarPay, in seconds.
    """

    def __init__(self, base_url: str, secret: str) -> None:
        """base_url is BarPay's deployment, such as "https://barpay.store.example". It must be
        secure_url, so the shared secret is never sent where the network can read it.
        """
        if not _SECRET.fullmatch(secret):
            raise ValueError("the shared secret must be visible ASCII characters only")
        url = urllib.parse.urlsplit(base_url)
        if url.scheme not in ("http", "https") or not url.hostname:
            raise ValueError("BarPay's URL must be an http or https URL with a host")
        if not secure_url(base_url):
            raise ValueError(f"BarPay's URL {INSECURE_URL}")
        if url.username is not None or url.query or url.fragment:
            raise ValueError("BarPay's URL must not have a user, a query or a fragment")
        self._https = url.scheme == "https"
        self._host = url.hostname
        self._port = url.port
        self._prefix = url.path.rstrip("/") + "/v1/edge"
        self._secret = secret

    def __repr__(self) -> str:
        return f"Client(host={self._host!r})"

    def scan(self, token: str, *, timeout: float) -> ScanResult:
        """scanExitToken: report token, as read from the exit QR."""
        body = self._call("POST", "/exit-scans", {"token": token}, timeout, {200}, {400, 401})
        if not isinstance(body, dict):
            raise NoAnswer("BarPay's answer to the scan is not a ScanResult")
        unlock, unlock_id = body.get("unlock"), body.get("unlock_id")
        message = body.get("reader_message")
        if not isinstance(message, str) or message not in ReaderMessage:
            raise NoAnswer("BarPay's answer to the scan has no reader message edge.yaml defines")
        if not isinstance(unlock, bool) or (unlock and not _is_unlock_id(unlock_id)):
            raise NoAnswer("BarPay's answer to the scan is not a ScanResult")
        return ScanResult(unlock, unlock_id if unlock else None, ReaderMessage(message))

    def wait_for_unlock(self, *, timeout: float) -> str | None:
        """waitForUnlock: the ID of an unlock staff made by hand, or None when none came.

        BarPay waits up to UNLOCK_WAIT seconds before it answers, so timeout must be longer.
        """
        body = self._call("GET", "/unlocks/next", None, timeout, {200, 204}, {401})
        if body is None:
            return None
        unlock_id = body.get("unlock_id") if isinstance(body, dict) else None
        if not _is_unlock_id(unlock_id):
            raise NoAnswer("BarPay's answer to the wait for an unlock is not an Unlock")
        return unlock_id

    def report_unlock(self, unlock_id: str, unlocked: bool, *, timeout: float) -> None:
        """reportUnlockResult: whether the unlock worked. BarPay keeps the first report and answers
        a repeat the same way, so it is safe to send again after NoAnswer.
        """
        path = "/unlocks/" + urllib.parse.quote(unlock_id, safe="") + "/result"
        self._call("POST", path, {"unlocked": unlocked}, timeout, {204}, {400, 401, 404})

    def send_liveliness(self, liveliness: float, *, timeout: float) -> None:
        """sendSignals: the room's liveliness, from 0 (quiet) to 1 (loudest)."""
        if not math.isfinite(liveliness):
            raise ValueError("the liveliness must be a finite number")
        self._call("POST", "/signals", {"liveliness": liveliness}, timeout, {204}, {400, 401})

    def _call(
        self,
        method: str,
        path: str,
        payload: dict[str, Any] | None,
        timeout: float,
        successes: set[int],
        refusals: set[int],
    ) -> Any:
        """Send the request and return the decoded JSON of a 200, or None for a 204.

        successes and refusals are the statuses edge.yaml defines for the operation; any other
        status is NoAnswer.
        """
        try:
            status, data = self._send(method, path, payload, timeout)
        except (OSError, http.client.HTTPException) as e:
            # The cause is left out so a traceback shows only this text, from which the secret is
            # removed; its type tells a timeout from a refused connection.
            raise NoAnswer(self._scrub(f"BarPay did not answer: {type(e).__name__}: {e}")) from None
        if status in refusals:
            error = _error(data)
            if error is None:
                raise NoAnswer(f"BarPay answered {status} without edge.yaml's error body")
            code, message = error
            raise Refused(status, self._scrub(code), self._scrub(message))
        if status not in successes:
            raise NoAnswer(f"BarPay answered {status}, which edge.yaml does not define here")
        if status == 204:
            return None
        try:
            return json.loads(data)
        except (ValueError, RecursionError):
            # A body nested too deeply for the decoder is not an answer edge.yaml defines either.
            raise NoAnswer("BarPay's answer is not JSON") from None

    def _send(
        self, method: str, path: str, payload: dict[str, Any] | None, timeout: float
    ) -> tuple[int, bytes]:
        conn: http.client.HTTPConnection
        if self._https:
            conn = http.client.HTTPSConnection(self._host, self._port, timeout=timeout)
        else:
            conn = http.client.HTTPConnection(self._host, self._port, timeout=timeout)
        headers = {"Authorization": "Bearer " + self._secret, "Accept": "application/json"}
        body = None
        if payload is not None:
            body = json.dumps(payload, allow_nan=False).encode()
            headers["Content-Type"] = "application/json"
        try:
            # http.client follows no redirect, so the secret goes to BarPay's URL only.
            conn.request(method, self._prefix + path, body=body, headers=headers)
            resp = conn.getresponse()
            return resp.status, resp.read(_MAX_RESPONSE_SIZE)
        finally:
            conn.close()

    def _scrub(self, text: str) -> str:
        """Text from outside the client could quote the secret back, such as a server echoing the
        Authorization header, so the secret is taken out before the text goes into an error.
        """
        return text.replace(self._secret, "[shared secret]")


def _is_unlock_id(value: object) -> TypeIs[str]:
    return isinstance(value, str) and _UNLOCK_ID.fullmatch(value) is not None


def _error(data: bytes) -> tuple[str, str] | None:
    """The code and message of edge.yaml's Error body, or None when data is not one."""
    try:
        body = json.loads(data)
    except (ValueError, RecursionError):
        return None
    err = body.get("error") if isinstance(body, dict) else None
    if not isinstance(err, dict):
        return None
    code, message = err.get("code"), err.get("message")
    if not isinstance(code, str) or not isinstance(message, str):
        return None
    return code, message
