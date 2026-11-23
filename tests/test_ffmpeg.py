import os
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

from telethon.extensions import BinaryReader
from telethon.tl.types import DocumentAttributeVideo

from tools.tool import UserAbort, configure_ffmpeg, external_ffmpeg
from tools.upload_file import upload_file


class FFmpegConfigTests(unittest.TestCase):
    def test_uses_external_executable_from_path(self):
        with patch.dict(os.environ, {}, clear=True), patch('tools.tool.shutil.which', return_value='ffmpeg') as which:
            configure_ffmpeg()
            which.assert_called_once_with('ffmpeg')
            self.assertEqual(os.environ['FFMPEG_BINARY'], str(Path('ffmpeg').absolute()))

    def test_honors_explicit_executable(self):
        custom_path = str(Path('custom tools', 'ffmpeg').absolute())
        with patch.dict(os.environ, {'FFMPEG_BINARY': custom_path}), \
                patch('tools.tool.shutil.which', return_value=custom_path) as which:
            configure_ffmpeg()
            which.assert_called_once_with(custom_path)
            self.assertEqual(os.environ['FFMPEG_BINARY'], custom_path)

    def test_missing_executable_reports_installation_hint(self):
        with patch.dict(os.environ, {}, clear=True), patch('tools.tool.shutil.which', return_value=None):
            with self.assertRaisesRegex(UserAbort, '请安装 FFmpeg'):
                configure_ffmpeg()


class FFmpegRuntimeTests(unittest.TestCase):
    def test_frozen_linux_hides_bundled_libraries_and_restores_path(self):
        for original in ({}, {'LD_LIBRARY_PATH_ORIG': '/opt/system/lib'}):
            with self.subTest(original=original), \
                    patch.dict(os.environ, {'LD_LIBRARY_PATH': '/tmp/_MEI123', **original}, clear=True), \
                    patch.object(sys, 'platform', 'linux'), patch.object(sys, 'frozen', True, create=True), \
                    patch('tools.tool.configure_ffmpeg'):
                with external_ffmpeg():
                    self.assertEqual(os.environ.get('LD_LIBRARY_PATH'), original.get('LD_LIBRARY_PATH_ORIG'))
                self.assertEqual(os.environ['LD_LIBRARY_PATH'], '/tmp/_MEI123')

    def test_frozen_linux_restores_environment_on_failure(self):
        with patch.dict(os.environ, {'LD_LIBRARY_PATH_ORIG': '/opt/system/lib'}, clear=True), \
                patch.object(sys, 'platform', 'linux'), patch.object(sys, 'frozen', True, create=True), \
                patch('tools.tool.configure_ffmpeg'):
            with self.assertRaisesRegex(OSError, 'probe failed'), external_ffmpeg():
                self.assertEqual(os.environ['LD_LIBRARY_PATH'], '/opt/system/lib')
                raise OSError('probe failed')
            self.assertNotIn('LD_LIBRARY_PATH', os.environ)

    def test_source_linux_and_frozen_windows_keep_their_environment(self):
        for platform, frozen in (('linux', False), ('win32', True)):
            with self.subTest(platform=platform), patch.dict(os.environ, {'LD_LIBRARY_PATH': '/opt/user/lib'}), \
                    patch.object(sys, 'platform', platform), patch.object(sys, 'frozen', frozen, create=True), \
                    patch('tools.tool.configure_ffmpeg'):
                with external_ffmpeg():
                    self.assertEqual(os.environ['LD_LIBRARY_PATH'], '/opt/user/lib')


class UploadFFmpegTests(unittest.IsolatedAsyncioTestCase):
    async def test_uploading_regular_file_does_not_require_ffmpeg(self):
        client = AsyncMock()
        with tempfile.TemporaryDirectory() as temp_dir:
            sample = Path(temp_dir, 'sample.txt')
            sample.write_text('test', encoding='utf-8')
            with patch('tools.upload_file.find_spec', return_value=object()), \
                    patch('tools.upload_file.get_filetype', return_value='text/plain'), \
                    patch('tools.tool.configure_ffmpeg') as configure:
                await upload_file(client, 'me', str(sample), False, None)
        configure.assert_not_called()
        client.upload_file.assert_awaited_once()
        client.send_file.assert_awaited_once()

    async def test_missing_ffmpeg_does_not_upload_or_delete_video(self):
        client = AsyncMock()
        with tempfile.TemporaryDirectory() as temp_dir:
            sample = Path(temp_dir, 'sample.mp4')
            sample.write_bytes(b'test')
            with patch('tools.upload_file.find_spec', return_value=object()), \
                    patch('tools.upload_file.get_filetype', return_value='video/mp4'), \
                    patch('tools.tool.configure_ffmpeg', side_effect=UserAbort('找不到可执行的 FFmpeg')):
                with self.assertRaisesRegex(UserAbort, 'FFmpeg'):
                    await upload_file(client, 'me', str(sample), True, None)
            self.assertTrue(sample.exists())
        client.upload_file.assert_not_awaited()
        client.send_file.assert_not_awaited()

    async def test_fractional_duration_survives_thumbnail_failure_and_serialization(self):
        client = AsyncMock()
        clip = MagicMock()
        clip.return_value.__enter__.return_value = SimpleNamespace(duration=0.5, size=(32, 64))
        with tempfile.TemporaryDirectory() as temp_dir:
            sample = Path(temp_dir, 'video.mp4')
            sample.write_bytes(b'test')
            with patch('tools.upload_file.find_spec', return_value=object()), \
                    patch('tools.upload_file.get_filetype', return_value='video/mp4'), \
                    patch('tools.tool.configure_ffmpeg'), \
                    patch.dict('sys.modules', {'moviepy': SimpleNamespace(VideoFileClip=clip)}), \
                    patch('tools.upload_file.get_thumb', side_effect=OSError('test thumbnail failure')):
                await upload_file(client, 'me', str(sample), False, None)
        clip.assert_called_once_with(str(sample), audio=False)
        client.upload_file.assert_awaited_once()
        client.send_file.assert_awaited_once()
        sent = client.send_file.await_args.kwargs
        self.assertIsNone(sent['thumb'])
        self.assertFalse(sent['force_document'])
        attribute, = sent['attributes']
        with BinaryReader(bytes(attribute)) as reader:
            restored = reader.tgread_object()
        self.assertIsInstance(restored, DocumentAttributeVideo)
        self.assertEqual((restored.duration, restored.w, restored.h), (0.5, 32, 64))

    async def test_failed_video_probe_sends_document_not_zero_duration_video(self):
        client = AsyncMock()
        with tempfile.TemporaryDirectory() as temp_dir:
            sample = Path(temp_dir, 'video.mp4')
            sample.write_bytes(b'test')
            with patch('tools.upload_file.find_spec', return_value=object()), \
                    patch('tools.upload_file.get_filetype', return_value='video/mp4'), \
                    patch('tools.upload_file.get_video_attributes', side_effect=ValueError('no duration')), \
                    patch('tools.upload_file.get_thumb') as get_thumb:
                await upload_file(client, 'me', str(sample), False, None)
        get_thumb.assert_not_called()
        client.send_file.assert_awaited_once()
        sent = client.send_file.await_args.kwargs
        self.assertTrue(sent['force_document'])
        self.assertIsNone(sent['attributes'])
