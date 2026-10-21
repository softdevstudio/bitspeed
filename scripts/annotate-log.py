#!/usr/bin/env python3
"""Report a CI log through annotations, because Claude sessions cannot download job logs.

Usage: annotate-log.py LEVEL TITLE FILE [LINES]. Emits the last LINES lines (default 200)
as one annotation, escaped as the workflow command syntax requires.
"""
import sys

level, title, path = sys.argv[1:4]
count = int(sys.argv[4]) if len(sys.argv) > 4 else 200
with open(path, encoding='utf-8', errors='replace') as handle:
    lines = handle.read().splitlines()[-count:]
text = '\n'.join(lines)[-60000:]
text = text.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')
print(f'::{level} title={title}::{text}')
