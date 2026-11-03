"""The exit gate (PRD: UC5, UC6 and UC7 on the edge device's side; edge.yaml).

Each exit token the QR reader reads is sent to BarPay with scanExitToken, and the gate does what
the answer says: it unlocks the lock once when told to and shows the answer through the
indicator. Meanwhile one waitForUnlock call is always waiting, so staff can open the gate by
hand. Each unlock, after a scan or by hand, is reported once with reportUnlockResult.

The values and rules are the PRD's System overview. An exit token is never written to the logs,
nor put in an error; neither is the shared secret, which only the client holds.
"""

import logging
import queue
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass

from barpay_edge.barpay import UNLOCK_WAIT, BarPayError, Client, NoAnswer, Refused
from barpay_edge.parts import GateState, Indicator, Lock, QrReader, QrReadError

log = logging.getLogger(__name__)

_NS_PER_S = 1_000_000_000

# The PRD's System overview. How long a scan waits for BarPay's answer, in seconds: BarPay may
# charge PayPal before it answers, and waits for each call to PayPal for at most 10 seconds.
SCAN_TIMEOUT = 60.0
# How many times a report of an unlock is sent in all, the pause before sending it again, and
# how long each send waits for BarPay's answer, in seconds. Only a report whose outcome is not
# known is sent again: BarPay keeps the first report and answers a repeat the same way.
REPORT_ATTEMPTS = 3
REPORT_PAUSE = 2.0
REPORT_TIMEOUT = 10.0
# A read of the same exit token as the last scan, within this long after its result was shown,
# is not sent: a reader reads the QR again and again while it is held in front of it. Shorter
# than the shortest result the indicator shows, and than the default unlock time, so a customer
# who scans again to have the gate opened again (UC5's alternative flow) is not held up.
REPEAT_IGNORE_NS = 3 * _NS_PER_S
# How long one waitForUnlock call waits for BarPay's answer, longer than BarPay holds it; and the
# pause before calling again after one failed, in seconds.
WAIT_TIMEOUT = UNLOCK_WAIT + 10.0
WAIT_PAUSE = 2.0

# How often the loops look whether the edge device is stopping, in seconds.
_POLL = 0.1


@dataclass(frozen=True)
class Timing:
    """The gate's time limits, the PRD's unless a test sets shorter ones."""

    scan_timeout: float = SCAN_TIMEOUT
    report_attempts: int = REPORT_ATTEMPTS
    report_pause: float = REPORT_PAUSE
    report_timeout: float = REPORT_TIMEOUT
    repeat_ignore_ns: int = REPEAT_IGNORE_NS
    wait_timeout: float = WAIT_TIMEOUT
    wait_pause: float = WAIT_PAUSE


class Gate:
    """The exit gate, run by run until stop is set. clock gives monotonic nanoseconds."""

    def __init__(
        self,
        barpay: Client,
        reader: QrReader,
        lock: Lock,
        indicator: Indicator,
        *,
        clock: Callable[[], int] = time.monotonic_ns,
        timing: Timing = Timing(),  # noqa: B008 - frozen, so one instance is safe to share
    ) -> None:
        self._barpay = barpay
        self._reader = reader
        self._lock = lock
        self._indicator = indicator
        self._clock = clock
        self._timing = timing
        # Held for each decision about a read and for each change of the lock and the
        # indicator, so the states are shown in the order the gate acted.
        self._acting = threading.Condition()
        # The token waiting to be scanned, and whether a scan is under way from the read until
        # its result is shown.
        self._pending: str | None = None
        self._scanning = False
        # The token of the last scan, and when its result was shown.
        self._last: tuple[str, int] | None = None
        self._reports: queue.Queue[tuple[str, bool]] = queue.Queue()

    def run(self, stop: threading.Event) -> None:
        self._indicator.show(GateState.WAITING_FOR_SCAN)
        threads = [
            threading.Thread(target=self._scan_loop, args=(stop,), name="gate-scan", daemon=True),
            threading.Thread(target=self._wait_loop, args=(stop,), name="gate-wait", daemon=True),
            threading.Thread(
                target=self._report_loop, args=(stop,), name="gate-report", daemon=True
            ),
        ]
        for t in threads:
            t.start()
        try:
            self._read_loop(stop)
        finally:
            with self._acting:
                self._acting.notify_all()
            for t in threads:
                t.join(timeout=1.0)

    def _read_loop(self, stop: threading.Event) -> None:
        """Takes every read as it comes, so one that comes while a scan waits for BarPay's answer
        is dropped rather than sent after it.
        """
        while not stop.is_set():
            try:
                token = self._reader.read(_POLL)
            except QrReadError as e:
                self._not_a_token(e)
                continue
            if token is not None:
                self._offer(token)

    def _not_a_token(self, error: QrReadError) -> None:
        with self._acting:
            if self._scanning:
                log.info("a QR read came while a scan waits for BarPay; it is dropped")
                return
            log.warning("a QR read is not an exit token, so it is not sent: %s", error)
            self._indicator.show(GateState.SHOW_NEW_QR)

    def _offer(self, token: str) -> None:
        with self._acting:
            if self._scanning:
                # The gate opens once for each answer; a read sent after it would open it again.
                log.info("a QR read came while a scan waits for BarPay; it is dropped")
                return
            if self._repeated(token):
                log.info("the same QR was read again just after its result; it is not sent")
                return
            self._pending = token
            self._scanning = True
            self._indicator.show(GateState.WORKING)
            self._acting.notify_all()

    def _repeated(self, token: str) -> bool:
        """Whether token is the last scan's, read before the end of the time to ignore it after
        its result; from exactly the end on, it is not.
        """
        if self._last is None:
            return False
        last, shown_at = self._last
        return token == last and self._clock() < shown_at + self._timing.repeat_ignore_ns

    def _scan_loop(self, stop: threading.Event) -> None:
        while True:
            with self._acting:
                while self._pending is None and not stop.is_set():
                    self._acting.wait(_POLL)
                if self._pending is None:
                    return
                token, self._pending = self._pending, None
            self._scan(token)

    def _scan(self, token: str) -> None:
        try:
            result = self._barpay.scan(token, timeout=self._timing.scan_timeout)
        except NoAnswer as e:
            log.warning("BarPay did not answer the scan, so the gate stays locked: %s", e)
            self._finish(token, GateState.BARPAY_UNREACHABLE)
            return
        except Refused as e:
            # The message is BarPay's own text, which could quote the request.
            log.error(
                "BarPay refused the scan with %s %s, so the gate stays locked", e.status, e.code
            )
            self._finish(token, GateState.BARPAY_UNREACHABLE)
            return
        except Exception as e:  # noqa: BLE001 - the gate must stay ready for the next scan
            log.error("the scan failed with %s, so the gate stays locked", type(e).__name__)
            self._finish(token, GateState.BARPAY_UNREACHABLE)
            return
        shown = GateState(result.reader_message.value)
        with self._acting:
            if result.unlock and result.unlock_id is not None:
                worked = self._lock.unlock()
                log.info(
                    "BarPay said to unlock (%s); the unlock worked: %s", result.unlock_id, worked
                )
                self._reports.put((result.unlock_id, worked))
                if not worked:
                    shown = GateState.UNLOCK_FAILED
            else:
                log.info("BarPay said to stay locked: %s", result.reader_message.value)
            self._finish(token, shown)

    def _finish(self, token: str, state: GateState) -> None:
        with self._acting:
            self._indicator.show(state)
            self._last = (token, self._clock())
            self._scanning = False

    def _wait_loop(self, stop: threading.Event) -> None:
        failing = False
        while not stop.is_set():
            try:
                unlock_id = self._barpay.wait_for_unlock(timeout=self._timing.wait_timeout)
            except BarPayError as e:
                if not failing:
                    log.warning(
                        "waiting for an unlock by hand failed, so staff cannot open the gate by "
                        "hand until it works again; trying again every %s s: %s",
                        self._timing.wait_pause,
                        e,
                    )
                    failing = True
                stop.wait(self._timing.wait_pause)
                continue
            if failing:
                log.info("waiting for an unlock by hand again")
                failing = False
            if unlock_id is not None:
                self._unlock_by_hand(unlock_id)

    def _unlock_by_hand(self, unlock_id: str) -> None:
        with self._acting:
            self._indicator.show(GateState.WORKING)
            worked = self._lock.unlock()
            log.info("staff unlocked by hand (%s); the unlock worked: %s", unlock_id, worked)
            self._reports.put((unlock_id, worked))
            self._indicator.show(GateState.UNLOCKED_BY_HAND if worked else GateState.UNLOCK_FAILED)

    def _report_loop(self, stop: threading.Event) -> None:
        """Reports from a thread of its own, so a report sent again never holds up the next scan
        or unlock by hand.
        """
        while not stop.is_set():
            try:
                unlock_id, worked = self._reports.get(timeout=_POLL)
            except queue.Empty:
                continue
            self._report(unlock_id, worked, stop)

    def _report(self, unlock_id: str, worked: bool, stop: threading.Event) -> None:
        attempts = self._timing.report_attempts
        for attempt in range(1, attempts + 1):
            try:
                self._barpay.report_unlock(unlock_id, worked, timeout=self._timing.report_timeout)
            except Refused as e:
                log.error(
                    "BarPay refused the report of %s with %s %s; it is not sent again",
                    unlock_id,
                    e.status,
                    e.code,
                )
                return
            except NoAnswer as e:
                if attempt == attempts:
                    log.error(
                        "the report of %s got no answer in %s sends, so BarPay may not know how "
                        "the unlock went: %s",
                        unlock_id,
                        attempts,
                        e,
                    )
                    return
                log.warning(
                    "the report of %s got no answer; sending it again in %s s: %s",
                    unlock_id,
                    self._timing.report_pause,
                    e,
                )
                if stop.wait(self._timing.report_pause):
                    return
            else:
                log.info("reported %s: the unlock worked: %s", unlock_id, worked)
                return
