import io
import os
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from telethon.errors import FileReferenceExpiredError
from telethon.tl.types import MessageMediaPhoto, PeerChannel

from main import build_parser, handle_download
from tools.down_file import DownloadResult, down_group, download_file, safe_path_component
from tools.monit import start_monitor
from tools.tool import UserAbort, get_history_message, parse_user_ids


async def stream(*messages):
    for message in messages:
        yield message


def photo(download, *, name='file.jpg', message_id=1):
    return SimpleNamespace(id=message_id, media=MessageMediaPhoto(), download_media=AsyncMock(side_effect=download),
                           file=SimpleNamespace(name=name, size=4))


async def write_media(output, **_):
    output.write(b'data')
    return output


class SenderFilterTests(unittest.IsolatedAsyncioTestCase):
    async def test_explicit_invalid_or_partial_filter_aborts(self):
        client = SimpleNamespace(get_entity=AsyncMock(side_effect=ValueError('not found')))
        self.assertIsNone(await parse_user_ids(client, None))
        for value in ('', ' , ', '0', '24,', '@missing', '24,@missing'):
            with self.subTest(value=value), self.assertRaises(UserAbort):
                await parse_user_ids(client, value)

    async def test_channel_sender_uses_marked_id(self):
        client = SimpleNamespace(get_entity=AsyncMock(return_value=PeerChannel(12345)))
        self.assertEqual(await parse_user_ids(client, '77|@channel'), {77, -1000000012345})

    async def test_history_preserves_missing_messages_and_rejects_unknown_senders(self):
        selected, unknown, other = [SimpleNamespace(sender_id=value) for value in (24, None, 77)]
        client = SimpleNamespace(iter_messages=lambda *_args, **_kw: stream(None, selected, unknown, other))
        with patch('tools.tool.get_chat_title', AsyncMock(return_value='channel')):
            for sender_ids, expected in (({24}, [None, selected]), (set(), [None]),
                                         (None, [None, selected, unknown, other])):
                with self.subTest(sender_ids=sender_ids):
                    _, messages = await get_history_message(client, 12345, {'ids': [1, 2, 3, 4]}, sender_ids)
                    self.assertEqual([item async for item in messages], expected)

    async def test_invalid_monitor_filter_does_not_register_handler(self):
        class Client:
            get_entity = AsyncMock(side_effect=[PeerChannel(12345), ValueError('not found')])

            def on(self, _event):
                raise AssertionError('invalid filter must not register a handler')

        with patch('tools.monit.get_chat_title', AsyncMock(return_value='channel')), self.assertRaises(UserAbort):
            await start_monitor(Client(), ['12345'], '@missing')


class DownloadTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.root = Path(self.enterContext(TemporaryDirectory())).resolve()
        self.enterContext(patch.dict(os.environ, {'save_path': str(self.root)}))
        self.stdout = self.enterContext(redirect_stdout(io.StringIO()))
        self.stderr = self.enterContext(redirect_stderr(io.StringIO()))

    def test_portable_components(self):
        expected = {'普通 文件.jpg': '普通 文件.jpg', 'CON.txt': '_CON.txt', 'LPT²': '_LPT²',
                    'a/b\\c:d': 'a_b_c_d', '..': '_', 'a. ': 'a'}
        for source, sanitized in expected.items():
            with self.subTest(source=source):
                self.assertEqual(safe_path_component(source), sanitized)

    async def test_media_and_alias_are_written_under_save_path(self):
        message = photo(write_media, name='folder\\actual.jpg')
        result = await download_file(object(), 'folder/channel', 12345, message)
        path = self.root / 'folder_channel-12345' / 'folder_actual.jpg'
        self.assertIs(result, DownloadResult.DOWNLOADED)
        self.assertEqual(path.read_bytes(), b'data')
        self.assertEqual(list(path.parent.iterdir()), [path])
        result = await download_file(object(), 'folder/channel', 12345, message)
        self.assertIs(result, DownloadResult.SKIPPED)
        self.assertEqual(message.download_media.await_count, 1)

    async def test_linked_subdirectory_is_not_used(self):
        with TemporaryDirectory() as other:
            try:
                (self.root / 'channel-12345').symlink_to(other, target_is_directory=True)
            except (OSError, NotImplementedError):
                self.skipTest('symlink creation is not permitted on this runner')
            message = photo(write_media)
            self.assertIs(await download_file(object(), 'channel', 12345, message), DownloadResult.FAILED)
            message.download_media.assert_not_awaited()
            self.assertEqual(list(Path(other).iterdir()), [])

    async def test_failure_keeps_no_partial_and_batch_continues(self):
        async def broken(output, **_):
            output.write(b'da')
            raise OSError('simulated write failure')

        first = photo(broken, name='failed.jpg')
        second = photo(write_media, name='good.jpg', message_id=2)
        with patch('tools.down_file.get_history_message', AsyncMock(return_value=('channel', stream(first, None, second)))):
            self.assertFalse(await down_group(object(), 12345, {'min_id': 0}, None, None))
        self.assertIn('下载成功 1，跳过 1，失败 1', self.stdout.getvalue())
        self.assertIn('simulated write failure', self.stderr.getvalue())
        self.assertEqual([p.name for p in (self.root / 'channel-12345').iterdir()], ['good.jpg'])

    async def test_photo_retry_restarts_partial_from_empty(self):
        async def expired(output, **_):
            output.write(b'incomplete')
            raise FileReferenceExpiredError(None)

        refreshed = photo(write_media)
        client = SimpleNamespace(get_entity=AsyncMock(return_value='channel'),
                                 get_messages=AsyncMock(return_value=refreshed))
        self.assertIs(await download_file(client, 'channel', 12345, photo(expired)), DownloadResult.DOWNLOADED)
        self.assertEqual((self.root / 'channel-12345' / 'file.jpg').read_bytes(), b'data')
        client.get_messages.assert_awaited_once_with('channel', ids=1)


class DownloadCLITests(unittest.IsolatedAsyncioTestCase):
    async def test_reports_failure_after_processing_other_channels(self):
        args = build_parser().parse_args(['download', '-id', '12345, 23456|34567'])
        with patch('main.down_group', AsyncMock(side_effect=[False, True, True])) as group:
            with self.assertRaisesRegex(UserAbort, '本次下载有失败项'):
                await handle_download(object(), args)
        self.assertEqual([call.args[1] for call in group.await_args_list], ['12345', '23456', '34567'])

    async def test_message_link_with_query_still_selects_its_message(self):
        args = build_parser().parse_args(['download', '-id', 'https://t.me/c/12345/67/89?single'])
        with patch('main.down_group', AsyncMock(return_value=True)) as group:
            await handle_download(object(), args)
        self.assertEqual(group.await_args.args[1:3], ('12345', {'ids': 89}))

    async def test_bad_message_link_is_not_success(self):
        args = build_parser().parse_args(['download', '-id', 'https://t.me/onlychannel'])
        with self.assertRaisesRegex(UserAbort, '无法解析 t.me 消息链接'):
            await handle_download(object(), args)
