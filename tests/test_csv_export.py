import csv
import io
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from telethon.tl import types

from tools.tool import print_all_channel, print_group


async def stream(*items):
    for item in items:
        yield item


def dialog(chat_id, name):
    channel = types.Channel(id=chat_id, title=name, photo=types.ChatPhotoEmpty(), date=datetime.now(timezone.utc))
    return SimpleNamespace(entity=channel, name=name)


def attachment(message_id, name, size, caption):
    return SimpleNamespace(id=message_id, media=types.MessageMediaDocument(),
                           file=SimpleNamespace(name=name, size=size), message=caption)


class CSVExportTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.root = Path(self.enterContext(TemporaryDirectory()))
        self.enterContext(redirect_stdout(io.StringIO()))

    def read_csv(self, filename):
        with (self.root / filename).open(encoding='utf-8', newline='') as f:
            return list(csv.reader(f))

    def output_in_tempdir(self, filename, *args, **kwargs):
        return (self.root / filename).open(*args, **kwargs)

    async def test_channels_keep_headers_sort_and_csv_quoting(self):
        client = SimpleNamespace(iter_dialogs=lambda: stream(
            dialog(12345, '中文频道'), dialog(23456, 'A,"频道"'), SimpleNamespace(entity=object())))
        with patch('tools.tool.open', self.output_in_tempdir, create=True):
            await print_all_channel(client)
        self.assertEqual(self.read_csv('全部频道.csv'), [
            ['ID', '频道名'], ['23456', 'A,"频道"'], ['12345', '中文频道'],
        ])

    async def test_messages_deduplicate_before_lexicographic_sort_and_keep_multiline_caption(self):
        caption = '描述,"第一行"\n第二行'
        messages = stream(
            attachment(10, '文档.txt', 1024, caption),
            attachment(9, '文档.txt', 1025, '显示大小相同，保留首条'),
            attachment(8, '文档.txt', 1024**2, None),
            attachment(12, '大文件', 2 * 1024**2, '另一条'),
        )
        with patch('tools.tool.get_history_message', AsyncMock(return_value=('频道', messages))), \
                patch('tools.tool.open', self.output_in_tempdir, create=True):
            await print_group(object(), '12345')
        self.assertEqual(self.read_csv('频道-12345.csv'), [
            ['链接', '文件名', '描述', '大小'],
            ['https://t.me/c/12345/8', '文档.txt', '', '1024.0KB'],
            ['https://t.me/c/12345/12', '大文件', '另一条', '2.0MB'],
            ['https://t.me/c/12345/10', '文档.txt', caption, '1.0KB'],
        ])

    async def test_empty_export_still_has_headers(self):
        with patch('tools.tool.get_history_message', AsyncMock(return_value=('频道', stream(None)))), \
                patch('tools.tool.open', self.output_in_tempdir, create=True):
            await print_group(object(), '12345')
        self.assertEqual(self.read_csv('频道-12345.csv'), [['链接', '文件名', '描述', '大小']])
