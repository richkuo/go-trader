"""Make backtest/ modules importable from backtest/tests/."""
import os
import sys

BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if BACKTEST_DIR not in sys.path:
    sys.path.insert(0, BACKTEST_DIR)
