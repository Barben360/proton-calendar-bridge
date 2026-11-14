#!/usr/bin/env python3
"""Writes a byte-reproducible zip: mkzip.py ARCHIVE EPOCH DIR NAME...

Entries are NAMEs from DIR (flat files), in the order given by sorting the
bytes of the names. Fixed per entry: modification time = EPOCH (UTC, DOS
resolution, so rounded down to an even second; EPOCH >= 315532800), Deflate
level 9, created-by Unix, no extra fields, no comment, external attributes
0755 for files whose mode has the owner-execute bit and 0644 otherwise.
Python's zipfile and the same zlib behave the same on a workstation and on
the runners; nothing here reads the clock, the umask or the file times.
Run it as python3 -I (package.sh does).
"""
import os
import sys
import time
import zipfile


def main(argv):
    if len(argv) < 5:
        sys.exit("usage: mkzip.py ARCHIVE EPOCH DIR NAME...")
    archive, epoch, src = argv[1], int(argv[2]), argv[3]
    names = sorted(argv[4:], key=lambda n: n.encode())
    if len(set(names)) != len(names):
        sys.exit("mkzip.py: duplicate names")
    when = time.gmtime(epoch)[:6]
    if when[0] < 1980:
        sys.exit("mkzip.py: EPOCH before 1980 cannot be stored in a zip")
    with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for name in names:
            if "/" in name or name in ("", ".", ".."):
                sys.exit("mkzip.py: not a flat file name: %r" % name)
            path = os.path.join(src, name)
            exe = os.stat(path).st_mode & 0o100
            info = zipfile.ZipInfo(name, date_time=when)
            info.create_system = 3
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (0o100755 if exe else 0o100644) << 16
            with open(path, "rb") as f:
                z.writestr(info, f.read(), zipfile.ZIP_DEFLATED, 9)


main(sys.argv)
