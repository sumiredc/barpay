"""The microphone, read through ALSA with pyalsaaudio (the `microphone` extra, Linux only).

What is read is held in memory only, as the block read() returns; nothing is written anywhere.
"""

import contextlib
import sys
from array import array
from collections.abc import Sequence
from typing import Any

from barpay_edge.parts import MicrophoneError

SAMPLE_RATE = 16000
# 0.1 seconds of samples per read.
BLOCK_SIZE = 1600
# Blocks ALSA buffers while the edge device is not reading; past that, they are lost.
_BUFFERED_BLOCKS = 10

_FULL_SCALE = 32768


class AlsaMicrophone:
    """Records one channel of 16-bit samples from an ALSA capture device, such as "default" or
    "hw:Loopback,1,0".

    The device is opened at the first read, and again at the read after one that failed, so the
    edge device keeps running while the microphone is unplugged and reads it once it is back.
    """

    def __init__(self, device: str) -> None:
        import alsaaudio  # type: ignore[import-not-found,import-untyped,unused-ignore]

        self._alsa: Any = alsaaudio
        self._device = device
        self._pcm: Any = None

    @property
    def sample_rate(self) -> int:
        return SAMPLE_RATE

    def read(self) -> Sequence[float]:
        try:
            if self._pcm is None:
                self._pcm = self._alsa.PCM(
                    type=self._alsa.PCM_CAPTURE,
                    mode=self._alsa.PCM_NORMAL,
                    rate=SAMPLE_RATE,
                    channels=1,
                    format=self._alsa.PCM_FORMAT_S16_LE,
                    periodsize=BLOCK_SIZE,
                    periods=_BUFFERED_BLOCKS,
                    device=self._device,
                )
            length, data = self._pcm.read()
        except self._alsa.ALSAAudioError as e:
            self._close()
            raise MicrophoneError(f"ALSA device {self._device!r}: {e}") from None
        if length < 0:
            # An overrun: ALSA dropped samples the edge device was too slow to read, and has
            # already recovered, so the next read goes on.
            return []
        return samples_from_s16le(data)

    def _close(self) -> None:
        pcm, self._pcm = self._pcm, None
        if pcm is not None:
            with contextlib.suppress(self._alsa.ALSAAudioError):
                pcm.close()


def samples_from_s16le(data: bytes) -> list[float]:
    """16-bit little-endian samples as numbers from -1 to 1."""
    samples = array("h")
    samples.frombytes(data[: len(data) - len(data) % 2])
    if sys.byteorder == "big":
        samples.byteswap()
    return [s / _FULL_SCALE for s in samples]
