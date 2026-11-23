import os
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

from tools.tool import get_filetype


class MagicDatabaseTests(unittest.TestCase):
    def test_frozen_linux_uses_bundled_database(self):
        with TemporaryDirectory() as temp:
            bundle_path = Path(temp, 'bundle')
            sample = Path(temp, 'sample.mp4')
            sample.write_bytes(b'test header')
            magic = MagicMock()
            magic.return_value.from_buffer.return_value = 'video/mp4'
            with patch.dict(sys.modules, {'magic': SimpleNamespace(Magic=magic)}), \
                    patch.dict(os.environ, {'MAGIC': '/host/magic.mgc'}), \
                    patch.object(sys, 'platform', 'linux'), \
                    patch.object(sys, 'frozen', True, create=True), \
                    patch.object(sys, '_MEIPASS', str(bundle_path), create=True):
                self.assertEqual(get_filetype(str(sample)), 'video/mp4')
                self.assertEqual(os.environ['MAGIC'], '/host/magic.mgc')
            magic.assert_called_once_with(mime=True, magic_file=str(bundle_path / 'teledown-magic' / 'magic.mgc'))
            magic.return_value.from_buffer.assert_called_once_with(b'test header')

    def test_source_linux_and_frozen_windows_keep_default_database(self):
        with TemporaryDirectory() as temp:
            sample = Path(temp, 'sample.txt')
            sample.write_bytes(b'test')
            for platform, frozen in (('linux', False), ('win32', True)):
                with self.subTest(platform=platform):
                    magic = MagicMock()
                    with patch.dict(sys.modules, {'magic': SimpleNamespace(Magic=magic)}), \
                            patch.object(sys, 'platform', platform), \
                            patch.object(sys, 'frozen', frozen, create=True):
                        get_filetype(str(sample))
                    magic.assert_called_once_with(mime=True, magic_file=None)
