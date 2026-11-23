import io
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from tools.tool import UserAbort
from tools.upload_file import upload_file


LIMIT = 2_097_152_000


class UploadSizeLimitTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.directory = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.client = AsyncMock()
        self.mime = self.enterContext(patch('tools.upload_file.get_filetype', return_value='text/plain'))
        self.enterContext(patch('tools.upload_file.find_spec', return_value=object()))
        self.enterContext(patch('tools.upload_file.TqdmUpTo'))
        self.sizes = {}
        self.enterContext(patch('tools.upload_file.os.path.getsize', side_effect=self.sizes.__getitem__))

    def sample(self, name: str, size: int) -> str:
        path = self.directory / name
        path.write_bytes(b'test')
        self.sizes[str(path)] = size
        return str(path)

    async def test_file_at_exact_limit_does_not_require_account_lookup(self):
        path = self.sample('sample.txt', LIMIT)
        await upload_file(self.client, 'me', path, False, None)
        self.client.get_me.assert_not_awaited()
        self.client.upload_file.assert_awaited_once()
        self.client.send_file.assert_awaited_once()

    async def test_oversized_file_is_rejected_before_media_probe_or_transfer(self):
        path = self.sample('sample.mp4', LIMIT + 1)
        for account in (SimpleNamespace(premium=None, bot=None), SimpleNamespace(premium=True, bot=True)):
            with self.subTest(account=account):
                self.client.get_me.return_value = account
                with self.assertRaisesRegex(UserAbort, r'拒绝上传 sample\.mp4.*2,097,152,000'):
                    await upload_file(self.client, 'me', path, True, None)
        self.mime.assert_not_called()
        self.client.upload_file.assert_not_awaited()
        self.client.send_file.assert_not_awaited()
        self.assertTrue(Path(path).is_file())

    async def test_premium_user_can_upload_above_normal_limit(self):
        self.client.get_me.return_value = SimpleNamespace(premium=True, bot=None)
        path = self.sample('sample.txt', LIMIT + 1)
        await upload_file(self.client, 'me', path, False, None)
        self.client.get_me.assert_awaited_once_with()
        self.client.upload_file.assert_awaited_once()
        self.client.send_file.assert_awaited_once()

    async def test_unknown_account_cannot_upload_above_normal_limit(self):
        self.client.get_me.return_value = None
        path = self.sample('sample.txt', LIMIT + 1)
        with self.assertRaisesRegex(UserAbort, '无法确认当前登录账号'):
            await upload_file(self.client, 'me', path, False, None)
        self.mime.assert_not_called()
        self.client.upload_file.assert_not_awaited()

    async def test_directory_skips_oversize_files_and_reports_partial_failure(self):
        self.client.get_me.return_value = SimpleNamespace(premium=False, bot=False)
        files = [self.sample('large1.txt', LIMIT + 1), self.sample('ok.txt', LIMIT),
                 self.sample('large2.txt', LIMIT + 2)]
        with patch('tools.upload_file.get_all_files', return_value=files), redirect_stderr(io.StringIO()) as stderr:
            with self.assertRaisesRegex(UserAbort, '2 个文件'):
                await upload_file(self.client, 'me', str(self.directory), True, None)
        self.client.get_me.assert_awaited_once_with()
        self.mime.assert_called_once_with(files[1])
        self.client.upload_file.assert_awaited_once()
        self.client.send_file.assert_awaited_once()
        self.assertIn('large1.txt', stderr.getvalue())
        self.assertIn('large2.txt', stderr.getvalue())
        self.assertTrue(Path(files[0]).is_file())
        self.assertFalse(Path(files[1]).exists())
        self.assertTrue(Path(files[2]).is_file())
