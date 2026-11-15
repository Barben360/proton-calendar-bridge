"""The MSI's licence screen: the repository's LICENSE as RTF.

    python3 -I scripts/release/licensertf.py LICENSE packaging/windows/license.rtf
    python3 -I scripts/release/licensertf.py --check LICENSE packaging/windows/license.rtf

The first form writes the RTF; --check writes nothing and exits 1 when the
file is not what the first form would write (make release-test). Paragraphs
are the blocks between blank lines, their lines joined by a space so the
dialog wraps them; \\, { and } are escaped, other characters beyond ASCII
written as \\uN?. Usage errors exit 2.
"""

import sys

HEADER = "{\\rtf1\\ansi\\ansicpg1252\\deff0\\uc1{\\fonttbl{\\f0\\fswiss\\fcharset0 Segoe UI;}}\\pard\\sa120\\f0\\fs18 "


def escape(text):
    out = []
    for ch in text:
        if ch in "\\{}":
            out.append("\\" + ch)
        elif ord(ch) < 0x80:
            out.append(ch)
        else:
            for unit in _utf16(ch):
                out.append("\\u%d?" % (unit - 0x10000 if unit >= 0x8000 else unit))
    return "".join(out)


def _utf16(ch):
    b = ch.encode("utf-16-le")
    return [int.from_bytes(b[i : i + 2], "little") for i in range(0, len(b), 2)]


def rtf(licence):
    paragraphs, current = [], []
    for line in licence.replace("\r\n", "\n").split("\n"):
        if line.strip():
            current.append(line.strip())
        elif current:
            paragraphs.append(" ".join(current))
            current = []
    if current:
        paragraphs.append(" ".join(current))
    return HEADER + "\\par\n".join(escape(p) for p in paragraphs) + "\\par\n}\n"


def main(argv):
    check = argv[:1] == ["--check"]
    if check:
        argv = argv[1:]
    if len(argv) != 2:
        print("usage: licensertf.py [--check] LICENSE OUT.rtf", file=sys.stderr)
        return 2
    with open(argv[0], encoding="utf-8") as f:
        want = rtf(f.read())
    if check:
        try:
            with open(argv[1], encoding="ascii", newline="") as f:
                got = f.read()
        except (OSError, UnicodeDecodeError) as e:
            print("licensertf: %s: %s" % (argv[1], e), file=sys.stderr)
            return 1
        if got != want:
            print(
                "licensertf: %s is not %s as RTF: run python3 -I scripts/release/licensertf.py %s %s"
                % (argv[1], argv[0], argv[0], argv[1]),
                file=sys.stderr,
            )
            return 1
        return 0
    with open(argv[1], "w", encoding="ascii", newline="") as f:
        f.write(want)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
