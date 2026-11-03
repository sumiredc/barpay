import json
import logging
import math
import os
import sys
import tempfile
import threading
import time
from collections.abc import Callable, Iterator, Sequence
from contextlib import contextmanager
from pathlib import Path
from typing import Any

import pytest
from fake_barpay import FakeBarPay, error, off_spec
from helpers import SECRET, leaks

from barpay_edge.barpay import Client
from barpay_edge.liveliness import SEND_INTERVAL, Meter, Sender, Tuning, Window, liveliness
from barpay_edge.parts import MicrophoneError

TUNING = Tuning(quiet_dbfs=-50.0, loudest_dbfs=-20.0)


def tone(dbfs: float, n: int = 100) -> list[float]:
    """A square wave whose loudness is dbfs."""
    amplitude = 10 ** (dbfs / 20)
    return [amplitude if i % 2 else -amplitude for i in range(n)]


SILENCE = [0.0] * 100


class FakeClock:
    def __init__(self, now: float = 1000.0) -> None:
        self.now = now

    def __call__(self) -> float:
        return self.now


class ScriptedMicrophone:
    """Gives each step in turn, one second of the fake clock per read: a block, or
    MicrophoneError for a read that fails. Once the steps run out it sets stop.
    """

    def __init__(
        self, clock: FakeClock, stop: threading.Event, steps: Sequence[Sequence[float] | None]
    ) -> None:
        self._clock = clock
        self._stop = stop
        self._steps = list(steps)

    @property
    def sample_rate(self) -> int:
        return 100

    def read(self) -> Sequence[float]:
        if not self._steps:
            self._stop.set()
            return []
        step = self._steps.pop(0)
        self._clock.now += 1.0
        if step is None:
            raise MicrophoneError("the fake microphone cannot be read")
        return step


FAIL = None


def meter_sends(steps: Sequence[Sequence[float] | None], tuning: Tuning = TUNING) -> list[float]:
    """The values a Meter with a 10 second interval hands on while reading steps."""
    clock, stop = FakeClock(), threading.Event()
    sent: list[float] = []
    mic = ScriptedMicrophone(clock, stop, steps)
    Meter(mic, tuning, clock, sent.append, interval=10.0, retry_delay=0).run(stop)
    return sent


def wait_until(condition: Callable[[], bool], timeout: float = 5.0) -> None:
    deadline = time.monotonic() + timeout
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError("timed out waiting")
        time.sleep(0.01)


@contextmanager
def running(sender: Sender) -> Iterator[None]:
    stop = threading.Event()
    thread = threading.Thread(target=sender.run, args=(stop,), daemon=True)
    thread.start()
    try:
        yield
    finally:
        stop.set()
        thread.join(timeout=5)


def requests_reach(barpay: FakeBarPay, n: int) -> Callable[[], bool]:
    return lambda: len(barpay.requests) >= n


def bodies(barpay: FakeBarPay) -> list[Any]:
    return [json.loads(r.body) for r in barpay.requests]


# The conversion


@pytest.mark.parametrize(
    ("samples", "expected"),
    [
        pytest.param(SILENCE, 0.0, id="silence"),
        pytest.param(tone(-50), 0.0, id="at-quiet"),
        pytest.param(tone(-20), 1.0, id="at-loudest"),
        pytest.param(tone(-35), 0.5, id="halfway"),
        pytest.param(tone(-27.5), 0.75, id="three-quarters"),
    ],
)
def test_the_loudness_is_mapped_in_a_line_from_quiet_to_loudest(
    samples: list[float], expected: float
) -> None:
    assert liveliness(samples, TUNING) == pytest.approx(expected, abs=1e-9)


@pytest.mark.parametrize(
    ("samples", "expected"),
    [
        pytest.param(tone(-60), 0.0, id="below-quiet"),
        pytest.param(tone(-50.001), 0.0, id="just-below-quiet"),
        pytest.param(tone(-19.999), 1.0, id="just-above-loudest"),
        pytest.param(tone(0), 1.0, id="full-scale"),
    ],
)
def test_a_loudness_outside_the_range_is_clamped_to_0_or_1(
    samples: list[float], expected: float
) -> None:
    assert liveliness(samples, TUNING) == expected


def test_the_loudness_is_the_rms_of_all_the_samples_in_dbfs() -> None:
    window = Window()
    window.add(tone(-20))
    window.add(SILENCE)

    # The mean of the squares is half that of tone(-20), 3 dB below it, not the mean of the two
    # blocks' liveliness (0.5).
    expected = (-20 - 10 * math.log10(2) + 50) / 30
    assert window.liveliness(TUNING) == pytest.approx(expected)


def test_no_samples_give_no_liveliness() -> None:
    assert liveliness([], TUNING) is None


@pytest.mark.parametrize(
    ("tuning", "expected"),
    [
        pytest.param(TUNING, 0.5, id="quiet-50-loudest-20"),
        pytest.param(Tuning(quiet_dbfs=-40, loudest_dbfs=-20), 0.25, id="quiet-40-loudest-20"),
        pytest.param(Tuning(quiet_dbfs=-60, loudest_dbfs=-30), 25 / 30, id="quiet-60-loudest-30"),
    ],
)
def test_the_tuning_changes_the_liveliness_of_the_same_sound(
    tuning: Tuning, expected: float
) -> None:
    assert liveliness(tone(-35), tuning) == pytest.approx(expected)


@pytest.mark.parametrize(
    ("quiet", "loudest"),
    [
        pytest.param(-20, -20, id="equal"),
        pytest.param(-20, -50, id="quiet-above-loudest"),
        pytest.param(-50, 1, id="loudest-above-full-scale"),
        pytest.param(math.nan, -20, id="nan"),
        pytest.param(-math.inf, -20, id="infinite"),
    ],
)
def test_a_tuning_that_cannot_map_loudness_to_0_to_1_is_rejected(
    quiet: float, loudest: float
) -> None:
    with pytest.raises(ValueError, match="loudness"):
        Tuning(quiet_dbfs=quiet, loudest_dbfs=loudest)


# Sending about every 10 seconds


def test_the_send_interval_is_the_prds_10_seconds() -> None:
    assert SEND_INTERVAL == 10.0


def test_one_value_is_handed_on_per_interval_from_that_intervals_samples() -> None:
    sent = meter_sends([tone(-27.5)] * 10 + [tone(-42.5)] * 10 + [tone(-20)] * 5)

    assert sent == pytest.approx([0.75, 0.25])


@pytest.mark.parametrize(("seconds", "sends"), [(9, 0), (10, 1), (11, 1), (19, 1), (20, 2)])
def test_a_value_is_handed_on_once_10_seconds_have_passed(seconds: int, sends: int) -> None:
    assert len(meter_sends([tone(-35)] * seconds)) == sends


def test_nothing_is_handed_on_while_the_microphone_cannot_be_read() -> None:
    assert meter_sends([FAIL] * 30) == []


def test_a_window_with_a_failed_read_is_dropped_and_the_next_starts_after_it() -> None:
    sent = meter_sends([tone(-20)] * 9 + [FAIL] + [tone(-42.5)] * 9 + [tone(-42.5)])

    # Only the 10 seconds read whole after the failure, none of the loud ones before it.
    assert sent == pytest.approx([0.25])


def test_a_window_without_samples_is_not_handed_on() -> None:
    assert meter_sends([[]] * 10) == []


def test_a_failing_microphone_is_logged_once_until_it_reads_again(
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.INFO)

    meter_sends([FAIL] * 5 + [SILENCE] + [FAIL] * 5 + [SILENCE])

    assert caplog.text.count("no liveliness is sent") == 2
    assert caplog.text.count("can be read again") == 2


# Sending to BarPay


def test_each_value_is_sent_once_with_only_the_liveliness(barpay: FakeBarPay) -> None:
    sender = Sender(Client(barpay.url, SECRET), timeout=1.0)
    with running(sender):
        sender.offer(0.25)
        wait_until(lambda: len(barpay.requests) == 1)
        sender.offer(0.75)
        wait_until(lambda: len(barpay.requests) == 2)

    assert [(r.method, r.path) for r in barpay.requests] == [("POST", "/v1/edge/signals")] * 2
    assert bodies(barpay) == [{"liveliness": 0.25}, {"liveliness": 0.75}]


def test_each_value_sent_is_logged_for_tuning(
    barpay: FakeBarPay, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO)
    sender = Sender(Client(barpay.url, SECRET), timeout=1.0)
    with running(sender):
        sender.offer(0.25)
        wait_until(lambda: "sent the liveliness" in caplog.text)

    assert "sent the liveliness 0.250" in caplog.text


@pytest.mark.parametrize(
    "failure",
    [
        pytest.param(off_spec(503), id="5xx"),
        pytest.param(off_spec(204, delay=0.5), id="timeout"),
        pytest.param(off_spec(204, hang_up=True), id="hang-up"),
        pytest.param(error(400, "invalid_request"), id="refused"),
    ],
)
def test_a_value_that_was_not_sent_is_not_sent_again(barpay: FakeBarPay, failure: Any) -> None:
    barpay.queue("sendSignals", failure)
    sender = Sender(Client(barpay.url, SECRET), timeout=0.2)
    with running(sender):
        sender.offer(0.25)
        wait_until(lambda: len(barpay.requests) == 1)
        time.sleep(0.5)
        sender.offer(0.75)
        wait_until(lambda: len(barpay.requests) == 2)
        time.sleep(0.3)

    assert bodies(barpay) == [{"liveliness": 0.25}, {"liveliness": 0.75}]


def test_a_value_still_waiting_when_a_newer_one_comes_is_dropped(barpay: FakeBarPay) -> None:
    barpay.queue("sendSignals", off_spec(204, delay=0.5))
    sender = Sender(Client(barpay.url, SECRET), timeout=2.0)
    with running(sender):
        sender.offer(0.25)
        wait_until(lambda: len(barpay.requests) == 1)
        sender.offer(0.5)
        sender.offer(0.75)
        wait_until(lambda: len(barpay.requests) == 2)
        time.sleep(0.3)

    assert bodies(barpay) == [{"liveliness": 0.25}, {"liveliness": 0.75}]


# Privacy and the shared secret

# Samples whose digits appear in nothing else the test makes.
AUDIO = 0.0123456789
AUDIO_DIGITS = "123456789"


class _FileWrites:
    """Records every file or directory the process creates or opens to write while on."""

    def __init__(self) -> None:
        self.on = False
        self.seen: list[str] = []
        sys.addaudithook(self._hook)

    def _hook(self, event: str, args: tuple[Any, ...]) -> None:
        if not self.on:
            return
        if event == "open":
            path, mode, flags = args
            writes = os.O_WRONLY | os.O_RDWR | os.O_CREAT | os.O_APPEND
            if (mode and any(c in mode for c in "wax+")) or (mode is None and flags & writes):
                self.seen.append(f"open {path!r}")
        elif event in ("os.mkdir", "tempfile.mkstemp", "tempfile.mkdtemp", "os.rename"):
            self.seen.append(f"{event} {args[0]!r}")


_FILE_WRITES = _FileWrites()


def test_the_audio_reaches_no_file_log_or_request(
    barpay: FakeBarPay,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path))
    caplog.set_level(logging.DEBUG)
    clock, stop = FakeClock(), threading.Event()
    block: Sequence[float] = [AUDIO if i % 2 else -AUDIO for i in range(100)]
    steps = [block] * 5 + [FAIL] + [block] * 10
    sender = Sender(Client(barpay.url, SECRET), timeout=1.0)

    _FILE_WRITES.on = True
    try:
        with running(sender):
            mic = ScriptedMicrophone(clock, stop, steps)
            Meter(mic, TUNING, clock, sender.offer, interval=10.0, retry_delay=0).run(stop)
            wait_until(lambda: len(barpay.requests) == 1)
    finally:
        _FILE_WRITES.on = False

    out = capsys.readouterr()
    assert _FILE_WRITES.seen == []
    assert list(tmp_path.iterdir()) == []
    assert AUDIO_DIGITS not in caplog.text + out.out + out.err
    (request,) = barpay.requests
    assert AUDIO_DIGITS not in request.path + request.body.decode()
    assert AUDIO_DIGITS not in "".join(request.headers.values())
    assert list(json.loads(request.body)) == ["liveliness"]


def test_the_secret_is_not_in_the_logs_when_a_send_fails(
    barpay: FakeBarPay,
    caplog: pytest.LogCaptureFixture,
    capsys: pytest.CaptureFixture[str],
) -> None:
    barpay.queue(
        "sendSignals",
        error(400, "invalid_request", f"got Bearer {SECRET}"),
        off_spec(500, f"Bearer {SECRET}".encode()),
        off_spec(204, hang_up=True),
    )
    caplog.set_level(logging.DEBUG)
    sender = Sender(Client(barpay.url, SECRET), timeout=1.0)
    with running(sender):
        for n in (1, 2, 3):
            sender.offer(0.5)
            wait_until(requests_reach(barpay, n))
        wait_until(lambda: caplog.text.count("was not sent") == 3)

    out = capsys.readouterr()
    assert not leaks(caplog.text, out.out, out.err)
