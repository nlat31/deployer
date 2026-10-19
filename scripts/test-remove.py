#!/usr/bin/env python3
"""Removal coverage shares the real launcher lifecycle fixture."""
import runpy
from pathlib import Path
runpy.run_path(str(Path(__file__).with_name('test-lifecycle.py')), run_name='__main__')
