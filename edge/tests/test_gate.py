"""The exit gate with a fake QR reader, a fake lock, a fake indicator and the fake BarPay (A)."""

import functools
import itertools
import json
import logging
import socket
import threading
import time
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass

import pytest
from fake_barpay import Answer, FakeBarPay, answer, error, off_spec
from helpers import SECRET, leaks

from barpay_edge.barpay import Client, ReaderMessage
from barpay_edge.fakes import FakeIndicator, FakeLock, FakeQrReader
from barpay_edge.gate import (
    REPEAT_IGNORE_NS,
    REPORT_ATTEMPTS,
    REPORT_PAUSE,
    REPORT_TIMEOUT,
    SCAN_TIMEOUT,
    WAIT_PAUSE,
    WAIT_TIMEOUT,
    Gate,
    Timing,
)
from barpay_edge.parts import GateState, QrReadError

TOKEN = "Ex1t-T0ken_with~all!kinds"
OTHER = "An0ther-exit-t0ken"
UNLOCK_ID = "glk_0123456789abcdef"
OTHER_UNLOCK_ID = "glk_fedcba9876543210"

# Short time limits so the tests do not wait the PRD's; the fake clock decides the time to
# ignore a repeated read.
FAST = Timing(
    scan_timeout=1.0,
    report_pause=0.05,
    report_timeout=1.0,
    wait_timeout=1.0,
    wait_pause=0.05,
)


class FakeClock:
    def __init__(self) -> None:
        self.now = 0

    def __call__(self) -> int:
        return self.now


@dataclass
class Running:
    barpay: FakeBarPay
    reader: FakeQrReader
    lock: FakeLock
    indicator: FakeIndicator
    clock: FakeClock

    def read(self, token: str | QrReadError) -> None:
        """The QR reader reads token, and the gate has taken it once this returns."""
        self.reader.feed(token)
        assert self.reader.wait_idle(5), "the gate did not take the read"

    def scans(self) -> list[str]:
        return [json.loads(r.body)["token"] for r in self.barpay.calls("scanExitToken")]

    def reports(self) -> list[tuple[str, bool]]:
        return [
            (r.path.split("/")[-2], json.loads(r.body)["unlocked"])
            for r in self.barpay.calls("reportUnlockResult")
        ]

    def results(self) -> list[GateState]:
        """The states shown after Waiting for a scan at the start."""
        shown = self.indicator.shown
        assert shown[0] == GateState.WAITING_FOR_SCAN
        return shown[1:]


def eventually(check: Callable[[], bool], timeout: float = 5.0) -> bool:
    deadline = time.monotonic() + timeout
    while not check():
        if time.monotonic() > deadline:
            return False
        time.sleep(0.005)
    return True


@contextmanager
def running(
    barpay: FakeBarPay,
    *,
    url: str | None = None,
    lock: FakeLock | None = None,
    timing: Timing = FAST,
) -> Iterator[Running]:
    """A gate running against barpay until the block ends."""
    r = Running(barpay, FakeQrReader(), lock or FakeLock(), FakeIndicator(), FakeClock())
    gate = Gate(
        Client(url or barpay.url, SECRET),
        r.reader,
        r.lock,
        r.indicator,
        clock=r.clock,
        timing=timing,
    )
    stop = threading.Event()
    thread = threading.Thread(target=gate.run, args=(stop,), daemon=True)
    thread.start()
    try:
        yield r
    finally:
        stop.set()
        thread.join(timeout=5)
        assert not thread.is_alive(), "the gate did not stop"


@pytest.fixture
def barpay(barpay: FakeBarPay) -> FakeBarPay:
    """The fake BarPay, holding each wait for an unlock by hand a little, as BarPay holds it."""
    barpay.unlock_wait = 0.05
    return barpay


def scan_answer(unlock: bool, message: ReaderMessage, unlock_id: str = UNLOCK_ID) -> Answer:
    body: dict[str, object] = {"unlock": unlock, "reader_message": message.value}
    if unlock:
        body["unlock_id"] = unlock_id
    return answer(200, body)


def test_the_gates_time_limits_are_the_prds() -> None:
    assert SCAN_TIMEOUT == 60.0
    assert (REPORT_ATTEMPTS, REPORT_PAUSE, REPORT_TIMEOUT) == (3, 2.0, 10.0)
    assert REPEAT_IGNORE_NS == 3_000_000_000
    assert (WAIT_TIMEOUT, WAIT_PAUSE) == (35.0, 2.0)
    assert Timing() == Timing(60.0, 3, 2.0, 10.0, 3_000_000_000, 35.0, 2.0)


def test_a_read_token_is_sent_to_barpay_as_read(barpay: FakeBarPay) -> None:
    with running(barpay) as g:
        g.read(TOKEN)

        assert eventually(lambda: len(g.results()) == 2)
        assert g.scans() == [TOKEN]


@pytest.mark.parametrize("message", list(ReaderMessage))
def test_each_reader_message_without_unlock_is_shown_and_the_lock_stays(
    barpay: FakeBarPay, message: ReaderMessage
) -> None:
    barpay.queue("scanExitToken", scan_answer(False, message))

    with running(barpay) as g:
        g.read(TOKEN)

        assert eventually(lambda: len(g.results()) == 2)
        assert g.results() == [GateState.WORKING, GateState(message.value)]
        assert g.lock.unlocks == 0
        assert g.reports() == []


def test_an_unlock_opens_the_lock_once_and_reports_it_worked(barpay: FakeBarPay) -> None:
    barpay.queue("scanExitToken", scan_answer(True, ReaderMessage.THANK_YOU))

    with running(barpay) as g:
        g.read(TOKEN)

        assert eventually(lambda: g.reports() != [])
        assert g.lock.unlocks == 1
        assert g.results() == [GateState.WORKING, GateState.THANK_YOU]
        time.sleep(0.2)  # time for a second report or unlock, which must not come
        assert g.reports() == [(UNLOCK_ID, True)]
        assert g.lock.unlocks == 1


def test_an_unlock_that_does_not_work_shows_unlock_failed_and_reports_it(
    barpay: FakeBarPay,
) -> None:
    barpay.queue("scanExitToken", scan_answer(True, ReaderMessage.THANK_YOU))

    with running(barpay, lock=FakeLock(fail=True)) as g:
        g.read(TOKEN)

        assert eventually(lambda: g.reports() != [])
        assert g.lock.unlocks == 1
        assert g.results() == [GateState.WORKING, GateState.UNLOCK_FAILED]
        assert g.reports() == [(UNLOCK_ID, False)]


def closed_port() -> int:
    """A port of the loopback address that nothing listens on."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port: int = s.getsockname()[1]
    return port


@pytest.mark.parametrize(
    "failure",
    [
        pytest.param("unreachable", id="not-reached"),
        pytest.param(off_spec(200, delay=1.5), id="no-answer-in-time"),
        pytest.param(off_spec(200, hang_up=True), id="hung-up"),
        pytest.param(off_spec(503, b"unavailable"), id="5xx"),
        pytest.param(off_spec(200, b"not json"), id="unreadable"),
        pytest.param(off_spec(200, b'{"unlock": true}'), id="not-a-scan-result"),
        pytest.param(error(401, "unauthorized"), id="refused"),
    ],
)
def test_a_scan_without_an_answer_keeps_the_lock_and_shows_barpay_unreachable(
    barpay: FakeBarPay, failure: Answer | str
) -> None:
    url = f"http://127.0.0.1:{closed_port()}" if failure == "unreachable" else None
    if isinstance(failure, Answer):
        barpay.queue("scanExitToken", failure)

    with running(barpay, url=url) as g:
        g.read(TOKEN)

        assert eventually(lambda: len(g.results()) == 2)
        assert g.results() == [GateState.WORKING, GateState.BARPAY_UNREACHABLE]
        assert g.lock.unlocks == 0


def test_a_scan_waits_for_barpays_answer_up_to_its_time_limit(barpay: FakeBarPay) -> None:
    barpay.queue(
        "scanExitToken", Answer(200, scan_answer(False, ReaderMessage.SEE_STAFF).body, delay=0.6)
    )

    with running(barpay) as g:
        g.read(TOKEN)

        assert eventually(lambda: len(g.results()) == 2)
        assert g.results() == [GateState.WORKING, GateState.SEE_STAFF]


@pytest.mark.parametrize(
    "problem",
    ["the read stopped before Enter", "the read is longer than 128 characters"],
)
def test_a_read_that_is_not_a_token_is_not_sent_and_asks_for_a_new_qr(
    barpay: FakeBarPay, problem: str
) -> None:
    with running(barpay) as g:
        g.read(QrReadError(problem))

        assert g.results() == [GateState.SHOW_NEW_QR]
        assert g.scans() == []


def test_the_same_token_is_not_sent_again_until_the_time_to_ignore_it_ends(
    barpay: FakeBarPay,
) -> None:
    with running(barpay) as g:
        g.read(TOKEN)
        assert eventually(lambda: len(g.results()) == 2)

        g.clock.now = REPEAT_IGNORE_NS - 1
        g.read(TOKEN)
        assert g.scans() == [TOKEN]
        assert len(g.results()) == 2

        g.clock.now = REPEAT_IGNORE_NS
        g.read(TOKEN)
        assert eventually(lambda: len(g.results()) == 4)
        assert g.scans() == [TOKEN, TOKEN]


def test_the_time_to_ignore_a_token_counts_from_when_its_result_was_shown(
    barpay: FakeBarPay,
) -> None:
    slow = Answer(200, scan_answer(False, ReaderMessage.SEE_STAFF).body, delay=0.2)
    barpay.queue("scanExitToken", slow)
    with running(barpay) as g:
        g.read(TOKEN)
        # BarPay answers after the clock has moved on by the time to ignore.
        g.clock.now = REPEAT_IGNORE_NS
        assert eventually(lambda: len(g.results()) == 2)

        g.clock.now = 2 * REPEAT_IGNORE_NS - 1
        g.read(TOKEN)
        assert g.scans() == [TOKEN]

        g.clock.now = 2 * REPEAT_IGNORE_NS
        g.read(TOKEN)
        assert eventually(lambda: len(g.scans()) == 2)


def test_another_token_is_sent_within_the_time_to_ignore_the_last(barpay: FakeBarPay) -> None:
    with running(barpay) as g:
        g.read(TOKEN)
        assert eventually(lambda: len(g.results()) == 2)

        g.read(OTHER)

        assert eventually(lambda: len(g.scans()) == 2)
        assert g.scans() == [TOKEN, OTHER]


def test_the_same_token_is_ignored_after_a_scan_barpay_did_not_answer_too(
    barpay: FakeBarPay,
) -> None:
    barpay.queue("scanExitToken", off_spec(503))
    with running(barpay) as g:
        g.read(TOKEN)
        assert eventually(lambda: len(g.results()) == 2)

        g.read(TOKEN)

        assert g.scans() == [TOKEN]


def test_a_read_while_barpay_has_not_answered_is_dropped(barpay: FakeBarPay) -> None:
    slow = Answer(200, scan_answer(True, ReaderMessage.THANK_YOU).body, delay=0.3)
    barpay.queue("scanExitToken", slow)

    with running(barpay) as g:
        g.read(TOKEN)
        g.read(OTHER)
        g.read(QrReadError("the read stopped before Enter"))
        g.read(TOKEN)

        assert eventually(lambda: g.reports() != [])
        time.sleep(0.2)  # time for a dropped read to be sent after all, which must not happen
        assert g.scans() == [TOKEN]
        assert g.lock.unlocks == 1
        assert g.results() == [GateState.WORKING, GateState.THANK_YOU]


def test_a_read_after_the_answer_is_sent(barpay: FakeBarPay) -> None:
    barpay.queue("scanExitToken", scan_answer(True, ReaderMessage.THANK_YOU))

    with running(barpay) as g:
        g.read(TOKEN)
        assert eventually(lambda: len(g.results()) == 2)

        g.read(OTHER)

        assert eventually(lambda: len(g.results()) == 4)
        assert g.scans() == [TOKEN, OTHER]


@pytest.mark.parametrize(
    ("answers", "sent"),
    [
        pytest.param([off_spec(503)] * 5, REPORT_ATTEMPTS, id="no-answer-every-time"),
        pytest.param([off_spec(503), answer(204)], 2, id="no-answer-then-kept"),
        pytest.param([off_spec(204, delay=1.5), answer(204)], 2, id="no-answer-in-time"),
        pytest.param([error(404, "unlock_not_found")], 1, id="refused-not-found"),
        pytest.param([error(400, "invalid_request")], 1, id="refused-invalid"),
        pytest.param([error(401, "unauthorized")], 1, id="refused-unauthorized"),
        pytest.param([answer(204)], 1, id="kept"),
    ],
)
def test_a_report_is_sent_again_only_while_its_outcome_is_not_known(
    barpay: FakeBarPay, answers: list[Answer], sent: int
) -> None:
    barpay.queue("scanExitToken", scan_answer(True, ReaderMessage.THANK_YOU))
    barpay.queue("reportUnlockResult", *answers)

    with running(barpay) as g:
        g.read(TOKEN)

        assert eventually(lambda: len(g.reports()) == sent)
        time.sleep(0.4)  # time for another send, which must not come
        assert g.reports() == [(UNLOCK_ID, True)] * sent


def test_a_report_is_sent_again_after_the_pause(barpay: FakeBarPay) -> None:
    barpay.queue("scanExitToken", scan_answer(True, ReaderMessage.THANK_YOU))
    barpay.queue("reportUnlockResult", off_spec(503), off_spec(503))
    timing = Timing(report_pause=0.3, wait_timeout=1.0)

    with running(barpay, timing=timing) as g:
        g.read(TOKEN)

        assert eventually(lambda: len(g.reports()) == 3)
        sends = [r.at for r in barpay.calls("reportUnlockResult")]
        assert all(b - a >= 0.3 for a, b in itertools.pairwise(sends))


def test_an_unlock_by_hand_opens_the_lock_once_and_reports_it(barpay: FakeBarPay) -> None:
    barpay.queue("waitForUnlock", answer(200, {"unlock_id": UNLOCK_ID}))

    with running(barpay) as g:
        assert eventually(lambda: g.reports() != [])
        assert g.lock.unlocks == 1
        assert g.results() == [GateState.WORKING, GateState.UNLOCKED_BY_HAND]
        time.sleep(0.2)  # time for a second unlock or report, which must not come
        assert g.reports() == [(UNLOCK_ID, True)]
        assert g.lock.unlocks == 1


def test_an_unlock_by_hand_that_does_not_work_shows_unlock_failed(barpay: FakeBarPay) -> None:
    barpay.queue("waitForUnlock", answer(200, {"unlock_id": UNLOCK_ID}))

    with running(barpay, lock=FakeLock(fail=True)) as g:
        assert eventually(lambda: g.reports() != [])
        assert g.results() == [GateState.WORKING, GateState.UNLOCK_FAILED]
        assert g.reports() == [(UNLOCK_ID, False)]


def test_the_gate_waits_again_at_once_after_no_unlock_came(barpay: FakeBarPay) -> None:
    barpay.unlock_wait = 0.0
    barpay.queue("waitForUnlock", answer(204), answer(200, {"unlock_id": UNLOCK_ID}))
    # A pause after a failure that would show if it were taken after a 204.
    timing = Timing(wait_pause=30.0, wait_timeout=1.0, report_pause=0.05)

    with running(barpay, timing=timing) as g:
        assert eventually(lambda: g.lock.unlocks == 1, timeout=2.0)


def test_the_gate_waits_again_at_once_after_an_unlock_by_hand(barpay: FakeBarPay) -> None:
    barpay.queue(
        "waitForUnlock",
        answer(200, {"unlock_id": UNLOCK_ID}),
        answer(200, {"unlock_id": OTHER_UNLOCK_ID}),
    )
    timing = Timing(wait_pause=30.0, wait_timeout=1.0, report_pause=0.05)

    with running(barpay, timing=timing) as g:
        assert eventually(lambda: len(g.reports()) == 2, timeout=2.0)
        assert sorted(g.reports()) == sorted([(UNLOCK_ID, True), (OTHER_UNLOCK_ID, True)])
        assert g.lock.unlocks == 2


@pytest.mark.parametrize(
    "failure",
    [
        pytest.param(off_spec(503), id="5xx"),
        pytest.param(off_spec(204, delay=1.5), id="no-answer-in-time"),
        pytest.param(error(401, "unauthorized"), id="refused"),
    ],
)
def test_the_gate_waits_again_after_the_pause_when_waiting_failed(
    barpay: FakeBarPay, failure: Answer
) -> None:
    barpay.queue("waitForUnlock", failure, answer(200, {"unlock_id": UNLOCK_ID}))
    timing = Timing(wait_pause=0.3, wait_timeout=1.0, report_pause=0.05)

    with running(barpay, timing=timing) as g:
        assert eventually(lambda: g.lock.unlocks == 1)
        first, second = barpay.calls("waitForUnlock")[:2]
        # A wait with no answer in time fails once its time limit has passed.
        failed_after = timing.wait_timeout if failure.delay else 0.0
        assert second.at - first.at >= failed_after + timing.wait_pause


def test_a_wait_for_an_unlock_by_hand_is_always_out(barpay: FakeBarPay) -> None:
    with running(barpay) as g:
        assert eventually(lambda: len(barpay.calls("waitForUnlock")) >= 3)
        assert g.lock.unlocks == 0


def test_no_token_and_no_secret_in_the_logs(
    barpay: FakeBarPay, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    barpay.queue(
        "scanExitToken",
        scan_answer(True, ReaderMessage.THANK_YOU),
        # BarPay's message is its own text, which a broken BarPay could fill with the request.
        error(400, "invalid_request", message=f"bad token {TOKEN} {SECRET}"),
        off_spec(503, f"{TOKEN} {SECRET}".encode()),
        off_spec(200, f"{TOKEN} {SECRET}".encode()),
    )
    barpay.queue("reportUnlockResult", off_spec(503), error(404, "unlock_not_found", TOKEN))
    barpay.queue("waitForUnlock", off_spec(503, f"{TOKEN} {SECRET}".encode()))

    with running(barpay) as g:
        for i in range(4):
            g.clock.now = i * REPEAT_IGNORE_NS
            g.read(TOKEN)
            shown = 2 * (i + 1)
            assert eventually(functools.partial(lambda n: len(g.results()) == n, shown))
        g.read(QrReadError("the read stopped before Enter"))
        assert eventually(lambda: len(g.reports()) == 2)

    assert not leaks(caplog.text)
    hidden = TOKEN not in caplog.text
    assert hidden, "an exit token is in the logs"


def test_the_gate_stops_while_a_scan_waits_for_barpay(barpay: FakeBarPay) -> None:
    barpay.queue(
        "scanExitToken", Answer(200, scan_answer(False, ReaderMessage.SEE_STAFF).body, delay=3)
    )

    with running(barpay) as g:
        g.read(TOKEN)
        assert eventually(lambda: g.scans() == [TOKEN])
    # running asserts that the gate stopped.
