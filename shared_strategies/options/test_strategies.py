"""Tests for options/strategies.py — options strategy registry and evaluation logic.

Options strategies are class-based and depend on DeribitOptionsAdapter + OptionsRiskManager.
We mock the adapter and risk manager to test strategy logic in isolation.
"""

import sys, os
sys.path.insert(0, os.path.dirname(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', '..', 'platforms', 'deribit'))

import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timedelta, timezone

from adapter import (
    OptionContract, OptionPosition, OptionType, OptionSide, Greeks,
)
from risk import OptionsRiskManager, OptionsRiskConfig
from strategies import (
    OPTIONS_STRATEGY_REGISTRY,
    list_options_strategies,
    get_options_strategy,
    create_options_strategy,
    MomentumOptionsStrategy,
    VolMeanReversionStrategy,
    ProtectivePutsStrategy,
    CoveredCallsStrategy,
)


# ─── Fixtures ───────────────────────────────

def _make_adapter():
    """Create a mock DeribitOptionsAdapter."""
    adapter = MagicMock()
    adapter.get_portfolio_value.return_value = 100_000.0
    adapter.get_positions.return_value = {}
    adapter.get_premium_at_risk.return_value = 0.0
    adapter.get_spot_price.return_value = 50_000.0
    adapter.get_iv_rank.return_value = 50.0
    adapter.get_portfolio_greeks.return_value = Greeks()
    adapter.get_open_position_count.return_value = 0
    return adapter


def _make_risk():
    """Create a real OptionsRiskManager with default config."""
    return OptionsRiskManager(OptionsRiskConfig())


def _make_contract(strike=50000.0, dte=30, option_type=OptionType.CALL,
                    underlying="BTC"):
    """Create an OptionContract. usd_price is derived from mid_price * spot_price."""
    # Set bid/ask so mid_price = 0.04, and spot_price = 50000 => usd_price = 2000
    return OptionContract(
        symbol=f"BTC-{strike}-C" if option_type == OptionType.CALL else f"BTC-{strike}-P",
        underlying=underlying,
        strike=strike,
        expiry=datetime.now(timezone.utc) + timedelta(days=dte),
        option_type=option_type,
        bid=0.03,
        ask=0.05,
        last=0.04,
        spot_price=50000.0,
        greeks=Greeks(delta=0.5, gamma=0.01, theta=-5.0, vega=100.0, iv=0.6),
    )


def _make_position(pid="pos1", underlying="BTC", option_type=OptionType.CALL,
                    side=OptionSide.BUY, strike=50000.0, pnl_pct=0.0, dte=20,
                    leg_group=None):
    pos = MagicMock(spec=OptionPosition)
    pos.underlying = underlying
    pos.option_type = option_type
    pos.side = side
    pos.strike = strike
    pos.pnl_pct = pnl_pct
    pos.dte = dte
    pos.leg_group = leg_group
    pos.quantity = 1.0
    pos.current_price = 0.04
    pos.entry_spot = 50000.0
    pos.current_spot = 50000.0
    return pid, pos


# ─── Registry ───────────────────────────────

class TestOptionsRegistry:
    def test_strategies_registered(self):
        names = list_options_strategies()
        assert len(names) >= 4
        for expected in ["momentum_options", "vol_mean_reversion",
                         "protective_puts", "covered_calls"]:
            assert expected in names

    def test_get_unknown_raises(self):
        with pytest.raises(ValueError, match="Unknown options strategy"):
            get_options_strategy("nonexistent")

    def test_create_strategy(self):
        adapter = _make_adapter()
        risk = _make_risk()
        strat = create_options_strategy("momentum_options", adapter, risk)
        assert isinstance(strat, MomentumOptionsStrategy)


# ─── Momentum Options ──────────────────────

class TestMomentumOptions:
    def test_no_signal_returns_none(self):
        adapter = _make_adapter()
        risk = _make_risk()
        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        # Mock _get_momentum_signal to return 0
        with patch.object(strat, '_get_momentum_signal', return_value=0):
            actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "none"

    def test_buy_signal_produces_call(self):
        adapter = _make_adapter()
        risk = _make_risk()
        contract = _make_contract()
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=1):
            actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "buy_call"
        assert actions[0]["contract"] == contract

    def test_sell_signal_produces_put(self):
        adapter = _make_adapter()
        risk = _make_risk()
        contract = _make_contract(option_type=OptionType.PUT)
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=-1):
            actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "buy_put"

    def test_existing_position_skips(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position()
        adapter.get_positions.return_value = {pid: pos}

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=1):
            actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"
        assert "Already have" in actions[0]["reason"]

    def test_no_options_found(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.find_options.return_value = []

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=1):
            actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"
        assert "No suitable calls" in actions[0]["reason"]

    def test_manage_positions_profit_target(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=55.0, side=OptionSide.BUY)
        adapter.get_positions.return_value = {pid: pos}

        strat = MomentumOptionsStrategy(adapter, risk, profit_target_pct=50.0,
                                         stop_loss_pct=30.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "close"
        assert "Profit target" in actions[0]["reason"]

    def test_manage_positions_stop_loss(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=-35.0, side=OptionSide.BUY)
        adapter.get_positions.return_value = {pid: pos}

        strat = MomentumOptionsStrategy(adapter, risk, profit_target_pct=50.0,
                                         stop_loss_pct=30.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "close"
        assert "Stop loss" in actions[0]["reason"]

    def test_manage_positions_approaching_expiry(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=10.0, side=OptionSide.BUY, dte=3)
        adapter.get_positions.return_value = {pid: pos}

        strat = MomentumOptionsStrategy(adapter, risk, profit_target_pct=50.0,
                                         stop_loss_pct=30.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "close"
        assert "expiry" in actions[0]["reason"].lower()


# ─── Volatility Mean Reversion ──────────────

class TestVolMeanReversion:
    def test_high_iv_sells_strangle(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.get_iv_rank.return_value = 85.0

        strat = VolMeanReversionStrategy(adapter, risk,
                                          high_iv_threshold=75, low_iv_threshold=25,
                                          target_dte=30, iv_lookback_days=60,
                                          exit_iv_reversion_pct=50.0, position_size_pct=5.0)
        actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "sell_strangle"

    def test_low_iv_buys_straddle(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.get_iv_rank.return_value = 15.0

        strat = VolMeanReversionStrategy(adapter, risk,
                                          high_iv_threshold=75, low_iv_threshold=25,
                                          target_dte=30, iv_lookback_days=60,
                                          exit_iv_reversion_pct=50.0, position_size_pct=5.0)
        actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "buy_straddle"

    def test_neutral_iv_no_action(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.get_iv_rank.return_value = 50.0

        strat = VolMeanReversionStrategy(adapter, risk,
                                          high_iv_threshold=75, low_iv_threshold=25,
                                          target_dte=30, iv_lookback_days=60,
                                          exit_iv_reversion_pct=50.0, position_size_pct=5.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"
        assert "neutral zone" in actions[0]["reason"]

    def test_existing_vol_position_skips(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.get_iv_rank.return_value = 85.0
        pid, pos = _make_position(leg_group="straddle_1")
        adapter.get_positions.return_value = {pid: pos}

        strat = VolMeanReversionStrategy(adapter, risk,
                                          high_iv_threshold=75, low_iv_threshold=25,
                                          target_dte=30, iv_lookback_days=60,
                                          exit_iv_reversion_pct=50.0, position_size_pct=5.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"
        assert "Already in vol trade" in actions[0]["reason"]

    def test_manage_sell_profit_target(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=55.0, side=OptionSide.SELL, leg_group="strangle_1")
        adapter.get_positions.return_value = {pid: pos}

        strat = VolMeanReversionStrategy(adapter, risk, exit_iv_reversion_pct=50.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "close_group"

    def test_manage_stop_loss(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=-35.0, side=OptionSide.SELL, leg_group="strangle_1")
        adapter.get_positions.return_value = {pid: pos}

        strat = VolMeanReversionStrategy(adapter, risk, exit_iv_reversion_pct=50.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert "stop loss" in actions[0]["reason"].lower()

    def test_manage_approaching_expiry(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=5.0, side=OptionSide.BUY,
                                   dte=5, leg_group="straddle_1")
        adapter.get_positions.return_value = {pid: pos}

        strat = VolMeanReversionStrategy(adapter, risk, exit_iv_reversion_pct=50.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert "expiry" in actions[0]["reason"].lower()


# ─── Protective Puts ────────────────────────

class TestProtectivePuts:
    def test_buys_otm_put(self):
        adapter = _make_adapter()
        risk = _make_risk()
        contract = _make_contract(strike=44000, dte=40, option_type=OptionType.PUT)
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract

        strat = ProtectivePutsStrategy(adapter, risk,
                                        otm_pct=12.0, target_dte=45, roll_dte=10,
                                        max_hedge_cost_pct=2.0, spot_holding_usd=5000.0)
        actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "buy_put"
        assert actions[0]["is_hedge"] is True

    def test_already_has_protective_puts(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(option_type=OptionType.PUT, side=OptionSide.BUY)
        adapter.get_positions.return_value = {pid: pos}

        strat = ProtectivePutsStrategy(adapter, risk,
                                        otm_pct=12.0, target_dte=45, roll_dte=10,
                                        max_hedge_cost_pct=2.0, spot_holding_usd=5000.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"
        assert "Already have" in actions[0]["reason"]

    def test_no_suitable_puts(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.find_options.return_value = []

        strat = ProtectivePutsStrategy(adapter, risk,
                                        otm_pct=12.0, target_dte=45, roll_dte=10,
                                        max_hedge_cost_pct=2.0, spot_holding_usd=5000.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"

    def test_manage_rolls_before_expiry(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(option_type=OptionType.PUT, side=OptionSide.BUY, dte=5)
        adapter.get_positions.return_value = {pid: pos}

        strat = ProtectivePutsStrategy(adapter, risk, roll_dte=10)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "roll"


# ─── Covered Calls ──────────────────────────

class TestCoveredCalls:
    def test_sells_otm_call(self):
        adapter = _make_adapter()
        risk = _make_risk()
        contract = _make_contract(strike=56000, dte=21, option_type=OptionType.CALL)
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract

        strat = CoveredCallsStrategy(adapter, risk,
                                      otm_pct=12.0, target_dte=21, roll_dte=5,
                                      itm_roll_threshold_pct=2.0, spot_holding_usd=5000.0)
        actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "sell_call"

    def test_already_has_covered_calls(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(option_type=OptionType.CALL, side=OptionSide.SELL)
        adapter.get_positions.return_value = {pid: pos}

        strat = CoveredCallsStrategy(adapter, risk,
                                      otm_pct=12.0, target_dte=21, roll_dte=5,
                                      itm_roll_threshold_pct=2.0, spot_holding_usd=5000.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"

    def test_no_suitable_calls(self):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.find_options.return_value = []

        strat = CoveredCallsStrategy(adapter, risk,
                                      otm_pct=12.0, target_dte=21, roll_dte=5,
                                      itm_roll_threshold_pct=2.0, spot_holding_usd=5000.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"

    def test_manage_roll_approaching_itm(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(option_type=OptionType.CALL, side=OptionSide.SELL,
                                   strike=50500, dte=15)
        adapter.get_positions.return_value = {pid: pos}

        strat = CoveredCallsStrategy(adapter, risk, roll_dte=5, itm_roll_threshold_pct=2.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "roll"
        assert "within" in actions[0]["reason"]

    def test_manage_roll_approaching_expiry(self):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(option_type=OptionType.CALL, side=OptionSide.SELL,
                                   strike=56000, dte=3)
        adapter.get_positions.return_value = {pid: pos}

        strat = CoveredCallsStrategy(adapter, risk, roll_dte=5, itm_roll_threshold_pct=2.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "roll"
        assert "DTE" in actions[0]["reason"]


# ─── Risk-blocked trades ────────────────────

class TestRiskBlocking:
    def test_momentum_blocked_by_risk(self):
        adapter = _make_adapter()
        risk = _make_risk()
        # Set max_positions to 0 to block
        risk.config.max_positions = 0
        # Need to return positions to trigger the position check before risk check
        # Actually: the risk check happens after find_options, so mock those
        contract = _make_contract()
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract
        adapter.get_positions.return_value = {}

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=1):
            actions = strat.evaluate("BTC")
        # Risk should block since max_positions=0
        assert actions[0]["type"] == "none"
        assert "Risk blocked" in actions[0]["reason"]

    def test_vol_strategy_blocked_by_risk(self):
        adapter = _make_adapter()
        risk = _make_risk()
        risk.config.max_positions = 0
        adapter.get_iv_rank.return_value = 85.0

        strat = VolMeanReversionStrategy(adapter, risk,
                                          high_iv_threshold=75, low_iv_threshold=25,
                                          target_dte=30, iv_lookback_days=60,
                                          exit_iv_reversion_pct=50.0, position_size_pct=5.0)
        actions = strat.evaluate("BTC")
        assert actions[0]["type"] == "none"
        assert "Risk blocked" in actions[0]["reason"]
