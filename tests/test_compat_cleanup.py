import io
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import AsyncMock, patch

from main import build_parser, handle_download
from tools.tool import get_history_message, parse_message_range
from tools.upload_file import upload_file


class RangeTests(unittest.TestCase):
    def test_supported_ranges(self):
        cases = {
            '>0': {'min_id': 0},
            '>100': {'min_id': 100},
            '<100': {'max_id': 100},
            '12345': {'ids': 12345},
            '=12345': {'ids': 12345},
            '1..100': {'min_id': 0, 'max_id': 101},
            '0..100': {'min_id': 0, 'max_id': 101},
            '100..100': {'min_id': 99, 'max_id': 101},
            '2147483647..2147483647': {'min_id': 2147483646, 'max_id': 0},
            '1, 5, 100': {'ids': [1, 5, 100], 'reverse': True},
        }
        for value, expected in cases.items():
            with self.subTest(value=value):
                self.assertEqual(parse_message_range(value), expected)

    def test_invalid_ranges(self):
        for value in ('', 'x123', '1..', '100..1', '0..0', '1..2147483648', '1,,2', '0', '1,0', '2147483648'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                parse_message_range(value)

    def test_default_range_is_parsed(self):
        args = build_parser().parse_args(['download', '-id', '12345'])
        self.assertEqual(args.range, {'min_id': 0})

    def test_legacy_ranges_are_rejected_by_cli(self):
        for value in ('s1s100', 's1..100'):
            with self.subTest(value=value), redirect_stderr(io.StringIO()) as stderr:
                with self.assertRaises(SystemExit) as error:
                    build_parser().parse_args(['download', '-id', '12345', '--range', value])
                self.assertEqual(error.exception.code, 2)
                self.assertIn('请改用 A..B', stderr.getvalue())


class DownloadDispatchTests(unittest.IsolatedAsyncioTestCase):
    async def test_link_selects_single_message(self):
        client = object()
        args = build_parser().parse_args(['download', '-id', 'https://t.me/c/12345/67/89'])
        with patch('main.down_group', new_callable=AsyncMock) as down_group:
            await handle_download(client, args)
        down_group.assert_awaited_once_with(client, '12345', {'ids': 89}, None, None)

    async def test_interval_sets_telethon_bounds_without_mutating_range(self):
        from unittest.mock import Mock

        client = Mock()
        message_range = parse_message_range('1..100')
        with patch('tools.tool.get_chat_title', new_callable=AsyncMock):
            await get_history_message(client, 12345, message_range, from_user_ids={10})
        client.iter_messages.assert_called_once_with(12345, min_id=0, max_id=101, from_user=10)
        self.assertEqual(message_range, {'min_id': 0, 'max_id': 101})

    async def test_missing_moviepy_reports_upload_unavailable(self):
        with patch.dict('sys.modules', {'moviepy': None}), redirect_stdout(io.StringIO()) as stdout:
            await upload_file(object(), 'me', 'unused', False, None)
        self.assertIn('当前版本未包含上传功能', stdout.getvalue())
