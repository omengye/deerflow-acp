"""Offline PCM normalization regressions; never call a cloud API or MQTT."""
import importlib.util
import math
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch
import wave

ROOT = Path(__file__).resolve().parents[1]
MODULE = ROOT/'src/sdk/stt_sdk/volcengine_asr2.py'
SPEC = importlib.util.spec_from_file_location('asr2_audiofix_test_module', MODULE)
asr = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = asr
SPEC.loader.exec_module(asr)
TEST_TEMP = ROOT.parents[3]/'.tmp/asr2-audiofix-tests'
TEST_TEMP.mkdir(parents=True, exist_ok=True)
tempfile.tempdir = str(TEST_TEMP)


def write_wav(path, *, rate=16000, frames=None, pcm=None, channels=1, width=2):
    if pcm is None:
        frames = frames if frames is not None else rate
        pcm = b'\x01\x02' * frames * channels if width == 2 else b'\x01' * frames * channels * width
    with wave.open(str(path), 'wb') as audio:
        audio.setnchannels(channels)
        audio.setsampwidth(width)
        audio.setframerate(rate)
        audio.writeframes(pcm)
    return path


class NormalizationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.path = Path(self.directory.name)/'audio.wav'

    def tearDown(self):
        self.directory.cleanup()

    def test_16k_pcm_is_bit_exact_and_never_invokes_resampler(self):
        pcm = bytes(range(256)) * 4
        write_wav(self.path, pcm=pcm)
        with patch.object(asr, '_resample_pcm') as resample, patch.object(asr.shutil, 'which') as which:
            actual = asr.read_pcm_wav(self.path)
        self.assertEqual(actual, pcm)
        self.assertFalse(actual.startswith(b'RIFF'))
        resample.assert_not_called()
        which.assert_not_called()

    def test_32k_recording_longer_than_60s_uses_source_size_and_duration(self):
        frames = 32000 * 62
        pcm = b'\x01\x02' * frames
        write_wav(self.path, rate=32000, pcm=pcm)
        self.assertGreater(self.path.stat().st_size, asr.MAX_AUDIO_BYTES + 65536)
        output = b'\x01\x02' * (16000 * 62)
        with patch.object(asr, '_resample_pcm', return_value=output) as resample:
            self.assertEqual(asr.read_pcm_wav(self.path), output)
        self.assertEqual(resample.call_args.args, (pcm, 32000))

    def test_120_second_limit_is_based_on_source_rate(self):
        for rate in (8000, 16000, 32000):
            with self.subTest(rate=rate):
                write_wav(self.path, rate=rate, frames=rate * 120)
                output = b'\x01\x02' * (16000 * 120)
                with patch.object(asr, '_resample_pcm', return_value=output) as resample:
                    self.assertEqual(len(asr.read_pcm_wav(self.path)), asr.MAX_AUDIO_BYTES)
                    self.assertEqual(resample.call_count, 0 if rate == 16000 else 1)
                write_wav(self.path, rate=rate, frames=rate * 120 + 1)
                with patch.object(asr, '_resample_pcm') as resample, self.assertRaises(asr.Asr2Error) as caught:
                    asr.read_pcm_wav(self.path)
                self.assertEqual(caught.exception.code, 'asr2_audio_duration_out_of_range')
                resample.assert_not_called()

    def test_unsupported_rate_channels_width_rejected_before_conversion(self):
        for options in ({'rate':44100}, {'channels':2}, {'width':1}, {'width':3}):
            with self.subTest(options=options):
                write_wav(self.path, frames=100, **options)
                with patch.object(asr, '_resample_pcm') as resample, self.assertRaises(asr.Asr2Error) as caught:
                    asr.read_pcm_wav(self.path)
                self.assertEqual(caught.exception.code, 'asr2_requires_supported_mono_s16_wav')
                resample.assert_not_called()

    def test_truncated_source_is_rejected_not_partially_recognized(self):
        write_wav(self.path, rate=32000, frames=32000)
        self.path.write_bytes(self.path.read_bytes()[:-3])
        with patch.object(asr, '_resample_pcm') as resample, self.assertRaises(asr.Asr2Error) as caught:
            asr.read_pcm_wav(self.path)
        self.assertEqual(caught.exception.code, 'asr2_truncated_audio')
        resample.assert_not_called()

    def test_excessive_file_bytes_rejected_before_parsing(self):
        self.path.write_bytes(b'0' * (asr.MAX_INPUT_AUDIO_BYTES + 65537))
        with self.assertRaises(asr.Asr2Error) as caught:
            asr.read_pcm_wav(self.path)
        self.assertEqual(caught.exception.code, 'asr2_invalid_audio_file')

    def test_missing_ffmpeg_is_safe_explicit_failure(self):
        with patch.object(asr.shutil, 'which', return_value=None), self.assertRaises(asr.Asr2Error) as caught:
            asr._resample_pcm(b'\x00\x00' * 3200, 32000)
        self.assertEqual(caught.exception.code, 'asr2_resampler_unavailable')

    def test_ffmpeg_failure_does_not_leak_stderr(self):
        failure = types.SimpleNamespace(returncode=1, stdout=b'', stderr=b'fixture-private-value')
        with patch.object(asr.shutil, 'which', return_value='/usr/bin/ffmpeg'), patch.object(asr.subprocess, 'run', return_value=failure), self.assertRaises(asr.Asr2Error) as caught:
            asr._resample_pcm(b'\x00\x00' * 3200, 32000)
        self.assertEqual(caught.exception.code, 'asr2_resample_failed')
        self.assertNotIn('fixture-private-value', str(caught.exception))

    def test_ffmpeg_timeout_is_bounded_safe_and_does_not_retry(self):
        failure = subprocess.TimeoutExpired('fixture-private-command', 10, stderr=b'fixture-private-value')
        with patch.object(asr.shutil, 'which', return_value='/usr/bin/ffmpeg'), patch.object(asr.subprocess, 'run', side_effect=failure) as run, self.assertRaises(asr.Asr2Error) as caught:
            asr._resample_pcm(b'\x00\x00' * 3200, 32000, timeout=90)
        self.assertEqual(caught.exception.code, 'asr2_resample_timeout')
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.kwargs['timeout'], asr.RESAMPLE_TIMEOUT_SECONDS)
        self.assertNotIn('fixture-private', str(caught.exception))

    def test_command_is_fixed_raw_pipe_and_output_duration_is_capped(self):
        pcm = b'\x01\x02' * 3200
        expected = b'\x03\x04' * 1600
        ok = types.SimpleNamespace(returncode=0, stdout=expected)
        with patch.object(asr.shutil, 'which', return_value='/usr/bin/ffmpeg'), patch.object(asr.subprocess, 'run', return_value=ok) as run:
            self.assertEqual(asr._resample_pcm(pcm, 32000, timeout=2), expected)
        argv = run.call_args.args[0]
        self.assertEqual(argv[argv.index('-i') + 1], 'pipe:0')
        self.assertEqual(argv[-1], 'pipe:1')
        self.assertEqual(argv[argv.index('-t') + 1], str(asr.MAX_AUDIO_SECONDS))
        self.assertEqual(run.call_args.kwargs['input'], pcm)
        self.assertEqual(run.call_args.kwargs['timeout'], 2)
        self.assertNotIn('shell', run.call_args.kwargs)

    def test_empty_odd_wrong_duration_or_oversized_output_is_rejected(self):
        pcm = b'\x01\x02' * 3200
        for output in (b'', b'\x01', b'\x00\x00' * 3200, b'\x00' * (asr.MAX_AUDIO_BYTES + 2)):
            with self.subTest(output_bytes=len(output)):
                ok = types.SimpleNamespace(returncode=0, stdout=output)
                with patch.object(asr.shutil, 'which', return_value='/usr/bin/ffmpeg'), patch.object(asr.subprocess, 'run', return_value=ok), self.assertRaises(asr.Asr2Error) as caught:
                    asr._resample_pcm(pcm, 32000)
                self.assertEqual(caught.exception.code, 'asr2_invalid_resampled_audio')

    @unittest.skipUnless(shutil.which('ffmpeg'), 'Real tone checks require installed FFmpeg')
    def test_real_32k_tone_preserves_duration_and_frequency(self):
        self.check_real_tone(32000)

    @unittest.skipUnless(shutil.which('ffmpeg'), 'Real tone checks require installed FFmpeg')
    def test_real_8k_tone_preserves_duration_and_frequency(self):
        self.check_real_tone(8000)

    def check_real_tone(self, rate):
        frequency, seconds = 1000, 1
        values = [round(12000 * math.sin(2 * math.pi * frequency * i / rate)) for i in range(rate * seconds)]
        pcm = struct.pack('<' + str(len(values)) + 'h', *values)
        write_wav(self.path, rate=rate, pcm=pcm)
        header_before = self.path.read_bytes()[:44]
        actual = asr.read_pcm_wav(self.path)
        # Merely relabeling the WAV header would yield wrong bytes/duration.
        self.assertEqual(len(actual), 16000 * seconds * 2)
        self.assertEqual(self.path.read_bytes()[:44], header_before)
        output = struct.unpack('<' + str(len(actual)//2) + 'h', actual)
        crossings = sum(left < 0 <= right for left, right in zip(output, output[1:]))
        estimated_frequency = crossings / (len(output) / 16000)
        self.assertAlmostEqual(estimated_frequency, frequency, delta=2)
        rms = math.sqrt(sum(x*x for x in output)/len(output))
        self.assertGreater(rms, 5000)


if __name__ == '__main__':
    unittest.main()
