import io
import unittest
from contextlib import redirect_stderr, redirect_stdout
from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

from telethon.client.messages import MessageMethods
from telethon.tl.types import InputPeerChannel
from telethon.tl.types.messages import Messages

from main import build_parser, handle_download, main
from tools.tool import get_history_message, parse_message_range
from tools.upload_file import upload_file


class RangeTests(unittest.TestCase):
    def test_supported_ranges(self):
        cases = {
            '>0': {'min_id': 0, 'reverse': True},
            '>100': {'min_id': 100, 'reverse': True},
            '>2147483647': {'min_id': 2147483647, 'max_id': 2147483647, 'reverse': True},
            '<100': {'max_id': 100, 'reverse': True},
            '12345': {'ids': 12345},
            '=12345': {'ids': 12345},
            '1..100': {'min_id': 0, 'max_id': 101, 'reverse': True},
            '0..100': {'min_id': 0, 'max_id': 101, 'reverse': True},
            '100..100': {'min_id': 99, 'max_id': 101, 'reverse': True},
            '2147483647..2147483647': {'min_id': 2147483646, 'max_id': 0, 'reverse': True},
            '100, 1, 5': {'ids': [1, 5, 100]},
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
        self.assertEqual(args.range, {'min_id': 0, 'reverse': True})

    def test_legacy_ranges_are_rejected_by_cli(self):
        for value in ('s1s100', 's1..100'):
            with self.subTest(value=value), redirect_stderr(io.StringIO()) as stderr:
                with self.assertRaises(SystemExit) as error:
                    build_parser().parse_args(['download', '-id', '12345', '--range', value])
                self.assertEqual(error.exception.code, 2)
                self.assertIn('请改用 A..B', stderr.getvalue())


class DateRangeTests(unittest.TestCase):
    def test_date_and_time_formats(self):
        parser = build_parser()
        base = ['download', '-id', '12345']
        for value in ('2026-09-01', '2026/09/01'):
            with self.subTest(value=value):
                args = parser.parse_args([*base, '--since', value, '--until', value])
                self.assertEqual(args.since, datetime(2026, 9, 1).astimezone(timezone.utc))
                self.assertEqual(args.until, datetime(2026, 9, 1, 23, 59, 59).astimezone(timezone.utc))
        for value in ('2026-09-01 12:30', '2026-09-01T12:30', '2026/09/01 12:30'):
            with self.subTest(value=value):
                args = parser.parse_args([*base, '--until', value])
                self.assertIsNone(args.since)
                self.assertEqual(args.until, datetime(2026, 9, 1, 12, 30).astimezone(timezone.utc))
        args = parser.parse_args([*base, '--since', '2026-09-01T12:30:01'])
        self.assertEqual(args.since, datetime(2026, 9, 1, 12, 30, 1).astimezone(timezone.utc))
        self.assertIsNone(args.until)

    def test_invalid_dates_and_reversed_range_fail_before_config(self):
        for options in (['--since', '2026-02-30'], ['--until', '2026-09-01T12:30Z'],
                        ['--since', '2026-09-02', '--until', '2026-09-01']):
            argv = ['teledown', 'download', '-id', '12345', *options]
            with self.subTest(options=options), patch('sys.argv', argv), patch('main.load_config') as load_config:
                with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                    main()
                self.assertEqual(error.exception.code, 2)
                load_config.assert_not_called()


class DownloadDispatchTests(unittest.IsolatedAsyncioTestCase):
    async def test_link_selects_single_message(self):
        client = object()
        args = build_parser().parse_args(['download', '-id', 'https://t.me/c/12345/67/89'])
        with patch('main.down_group', new_callable=AsyncMock) as down_group:
            await handle_download(client, args)
        down_group.assert_awaited_once_with(client, '12345', {'ids': 89}, None, None, since=None, until=None)

    async def test_message_link_also_passes_date_filter(self):
        client = object()
        args = build_parser().parse_args(['download', '-id', 'https://t.me/c/12345/89', '--since', '2026-09-01'])
        with patch('main.down_group', new_callable=AsyncMock) as group:
            await handle_download(client, args)
        group.assert_awaited_once_with(client, '12345', {'ids': 89}, None, None, since=args.since, until=None)

    async def test_interval_sets_telethon_bounds_without_mutating_range(self):
        client = Mock()
        message_range = parse_message_range('1..100')
        with patch('tools.tool.get_chat_title', new_callable=AsyncMock):
            await get_history_message(client, 12345, message_range, from_user_ids={10})
        client.iter_messages.assert_called_once_with(12345, min_id=0, max_id=101, reverse=True, from_user=10)
        self.assertEqual(message_range, {'min_id': 0, 'max_id': 101, 'reverse': True})

    async def test_all_range_forms_yield_low_to_high_with_real_telethon_iterator(self):
        class OfflineClient(MessageMethods):
            async def get_input_entity(self, chat_id):
                return InputPeerChannel(chat_id, 0)

            async def __call__(self, request):
                # Telegram returns each page in descending order. A negative add_offset
                # makes the request start on the newer side of the offset instead.
                if hasattr(request, 'id'):
                    ids = request.id
                elif request.add_offset < 0:
                    ids = range(92, max(80, request.offset_id) - 1, -1)
                else:
                    ids = range(min(93, request.offset_id) - 1 if request.offset_id else 92, 79, -1)
                return Messages(
                    messages=[SimpleNamespace(id=i, sender_id=None, _finish_init=Mock()) for i in ids],
                    chats=[], users=[], topics=[],
                )

        cases = {
            '>0': list(range(80, 93)),
            '>90': [91, 92],
            '<82': [80, 81],
            '<1': [],
            '>2147483647': [],
            '81..90': list(range(81, 91)),
            '90,81,85': [81, 85, 90],
            '81': [81],
            '=90': [90],
        }
        with patch('tools.tool.get_chat_title', new_callable=AsyncMock):
            for value, expected in cases.items():
                with self.subTest(value=value):
                    _, messages = await get_history_message(OfflineClient(), 12345, parse_message_range(value))
                    self.assertEqual([message.id async for message in messages], expected)

    async def test_missing_moviepy_reports_upload_unavailable(self):
        with patch.dict('sys.modules', {'moviepy': None}), redirect_stdout(io.StringIO()) as stdout:
            await upload_file(object(), 'me', 'unused', False, None)
        self.assertIn('当前版本未包含上传功能', stdout.getvalue())
