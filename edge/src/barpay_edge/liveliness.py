"""The room's liveliness: the microphone's loudness as a number from 0 (quiet) to 1 (loudest),
sent to BarPay about every 10 seconds (PRD: Pricing model, Signals; Guarantees, Privacy).

Samples stay in memory only: each block read is added to its window's sum of squares and then
dropped, so only that sum, the number of samples and the liveliness made from them outlive a
read, and only the liveliness leaves the edge device.
"""

import contextlib
import logging
import math
import queue
import threading
from collections.abc import Callable, Iterable
from dataclasses import dataclass

from barpay_edge.barpay import BarPayError, Client
from barpay_edge.parts import Microphone, MicrophoneError

# The PRD's Pricing model, Signals: the window each value is measured over, and how often one is
# sent, in seconds.
SEND_INTERVAL = 10.0

# The PRD's starting tuning, for a store that has not set its own.
DEFAULT_QUIET_DBFS = -50.0
DEFAULT_LOUDEST_DBFS = -20.0

# Bounds connecting and each read (Client), in seconds; well under SEND_INTERVAL, so a send that
# gets no answer is usually over before the next value is ready.
SEND_TIMEOUT = 5.0

# How long to wait before reading again after the microphone could not be read, in seconds.
RETRY_DELAY = 1.0

log = logging.getLogger("barpay_edge.liveliness")


@dataclass(frozen=True)
class Tuning:
    """A store's loudness, in dBFS, at which the liveliness is 0 and at which it is 1."""

    quiet_dbfs: float = DEFAULT_QUIET_DBFS
    loudest_dbfs: float = DEFAULT_LOUDEST_DBFS

    def __post_init__(self) -> None:
        if not (math.isfinite(self.quiet_dbfs) and math.isfinite(self.loudest_dbfs)):
            raise ValueError("the quiet and loudest loudness must be finite numbers")
        if not self.quiet_dbfs < self.loudest_dbfs <= 0:
            raise ValueError("the quiet loudness must be below the loudest, which is at most 0")


class Window:
    """The samples of one window, kept as their sum of squares and their number only."""

    def __init__(self) -> None:
        self._sum_of_squares = 0.0
        self._count = 0

    def add(self, samples: Iterable[float]) -> None:
        for s in samples:
            self._sum_of_squares += s * s
            self._count += 1

    def liveliness(self, tuning: Tuning) -> float | None:
        """The window's liveliness, or None when it has no samples."""
        if self._count == 0:
            return None
        rms = math.sqrt(self._sum_of_squares / self._count)
        if rms == 0:
            return 0.0
        dbfs = 20 * math.log10(rms)
        scaled = (dbfs - tuning.quiet_dbfs) / (tuning.loudest_dbfs - tuning.quiet_dbfs)
        return min(1.0, max(0.0, scaled))


def liveliness(samples: Iterable[float], tuning: Tuning) -> float | None:
    """The liveliness of samples, each from -1 to 1, or None when there are none."""
    window = Window()
    window.add(samples)
    return window.liveliness(tuning)


class Meter:
    """Reads the microphone and hands send one liveliness per window of SEND_INTERVAL seconds.

    clock gives monotonic seconds. A window ends with the first block read once the clock is
    interval or more past the window's start, and the next window starts then. A window with a
    failed read is dropped unsent, and the next one starts after waiting retry_delay seconds, so
    a value is only sent for a window read without a failed read. An overrun is not a failed
    read: the microphone gives an empty block for it.
    """

    def __init__(
        self,
        microphone: Microphone,
        tuning: Tuning,
        clock: Callable[[], float],
        send: Callable[[float], None],
        *,
        interval: float = SEND_INTERVAL,
        retry_delay: float = RETRY_DELAY,
    ) -> None:
        self._microphone = microphone
        self._tuning = tuning
        self._clock = clock
        self._send = send
        self._interval = interval
        self._retry_delay = retry_delay

    def run(self, stop: threading.Event) -> None:
        window, started = Window(), self._clock()
        failing = False
        while not stop.is_set():
            try:
                block = self._microphone.read()
            except MicrophoneError as e:
                if not failing:
                    log.warning("the microphone cannot be read; no liveliness is sent: %s", e)
                    failing = True
                stop.wait(self._retry_delay)
                window, started = Window(), self._clock()
                continue
            if failing:
                log.info("the microphone can be read again")
                failing = False
            window.add(block)
            now = self._clock()
            if now - started >= self._interval:
                value = window.liveliness(self._tuning)
                if value is not None:
                    self._send(value)
                window, started = Window(), now


class Sender:
    """Sends each liveliness offered to it to BarPay once, from its own thread, so a slow BarPay
    never holds up the microphone.

    A value that was not sent is not sent again: BarPay uses a value for 1 minute after it
    arrives, so a late one would stand for a room that has changed. For the same reason a value
    still waiting when a newer one is offered is dropped.
    """

    def __init__(self, client: Client, *, timeout: float = SEND_TIMEOUT) -> None:
        self._client = client
        self._timeout = timeout
        self._next: queue.Queue[float] = queue.Queue(maxsize=1)
        self._mu = threading.Lock()

    def offer(self, value: float) -> None:
        with self._mu:
            with contextlib.suppress(queue.Empty):
                self._next.get_nowait()
            self._next.put_nowait(value)

    def run(self, stop: threading.Event) -> None:
        while not stop.is_set():
            try:
                value = self._next.get(timeout=0.05)
            except queue.Empty:
                continue
            try:
                self._client.send_liveliness(value, timeout=self._timeout)
            except BarPayError as e:
                log.warning("the liveliness %.3f was not sent, and is not sent again: %s", value, e)
            else:
                log.info("sent the liveliness %.3f", value)
