
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', '..', 'platforms', 'deribit'))

import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timedelta, timezone

from shared_tools.conftest import load_module

_OPTIONS_DIR = os.path.dirname(os.path.abspath(__file__))
_DERIBIT_DIR = os.path.join(_OPTIONS_DIR, "..", "..", "platforms", "deribit")
_SAVED_MODULES = {name: sys.modules.get(name) for name in ("adapter", "risk")}
_ADAPTER = load_module("_options_deribit_adapter_test", os.path.join(_DERIBIT_DIR, "adapter.py"))
sys.modules["adapter"] = _ADAPTER
_RISK = load_module("_options_risk_test", os.path.join(_OPTIONS_DIR, "risk.py"))
sys.modules["risk"] = _RISK
_STRATEGIES = load_module("_options_strategies_test", os.path.join(_OPTIONS_DIR, "strategies.py"))
for _name, _module in _SAVED_MODULES.items():
    if _module is None:
        sys.modules.pop(_name, None)
    else:
        sys.modules[_name] = _module

OptionContract = _ADAPTER.OptionContract
OptionPosition = _ADAPTER.OptionPosition
OptionType = _ADAPTER.OptionType
OptionSide = _ADAPTER.OptionSide
Greeks = _ADAPTER.Greeks
OptionsRiskManager = _RISK.OptionsRiskManager
OptionsRiskConfig = _RISK.OptionsRiskConfig
OPTIONS_STRATEGY_REGISTRY = _STRATEGIES.OPTIONS_STRATEGY_REGISTRY
list_options_strategies = _STRATEGIES.list_options_strategies
get_options_strategy = _STRATEGIES.get_options_strategy
create_options_strategy = _STRATEGIES.create_options_strategy
MomentumOptionsStrategy = _STRATEGIES.MomentumOptionsStrategy
VolMeanReversionStrategy = _STRATEGIES.VolMeanReversionStrategy
ProtectivePutsStrategy = _STRATEGIES.ProtectivePutsStrategy
CoveredCallsStrategy = _STRATEGIES.CoveredCallsStrategy


def _make_adapter():
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
    return OptionsRiskManager(OptionsRiskConfig())


def _make_contract(strike=50000.0, dte=30, option_type=OptionType.CALL,
                    underlying="BTC"):
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


class TestMomentumOptions:
    @pytest.mark.parametrize("signal,option_type,expected", [
        (0, OptionType.CALL, "none"),
        (1, OptionType.CALL, "buy_call"),
        (-1, OptionType.PUT, "buy_put"),
    ])
    def test_signal_selects_action(self, signal, option_type, expected):
        adapter = _make_adapter()
        risk = _make_risk()
        contract = _make_contract(option_type=option_type)
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=signal):
            actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == expected
        if expected != "none":
            assert actions[0]["contract"] == contract

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

    @pytest.mark.parametrize("pnl_pct,dte,reason_substring", [
        (55.0, 20, "profit target"),
        (-35.0, 20, "stop loss"),
        (10.0, 3, "expiry"),
    ])
    def test_manage_positions_closes(self, pnl_pct, dte, reason_substring):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(pnl_pct=pnl_pct, side=OptionSide.BUY, dte=dte)
        adapter.get_positions.return_value = {pid: pos}

        strat = MomentumOptionsStrategy(adapter, risk, profit_target_pct=50.0,
                                         stop_loss_pct=30.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "close"
        assert reason_substring in actions[0]["reason"].lower()


class TestVolMeanReversion:
    @pytest.mark.parametrize("iv_rank,expected", [
        (85.0, "sell_strangle"),
        (15.0, "buy_straddle"),
        (50.0, "none"),
    ])
    def test_iv_rank_selects_action(self, iv_rank, expected):
        adapter = _make_adapter()
        risk = _make_risk()
        adapter.get_iv_rank.return_value = iv_rank

        strat = VolMeanReversionStrategy(adapter, risk,
                                          high_iv_threshold=75, low_iv_threshold=25,
                                          target_dte=30, iv_lookback_days=60,
                                          exit_iv_reversion_pct=50.0, position_size_pct=5.0)
        actions = strat.evaluate("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == expected
        if expected == "none":
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

    @pytest.mark.parametrize("strike,dte,reason_substring", [
        (50500, 15, "within"),
        (56000, 3, "DTE"),
    ])
    def test_manage_rolls(self, strike, dte, reason_substring):
        adapter = _make_adapter()
        risk = _make_risk()
        pid, pos = _make_position(option_type=OptionType.CALL, side=OptionSide.SELL,
                                   strike=strike, dte=dte)
        adapter.get_positions.return_value = {pid: pos}

        strat = CoveredCallsStrategy(adapter, risk, roll_dte=5, itm_roll_threshold_pct=2.0)
        actions = strat.manage_positions("BTC")
        assert len(actions) == 1
        assert actions[0]["type"] == "roll"
        assert reason_substring in actions[0]["reason"]


class TestRiskBlocking:
    def test_momentum_blocked_by_risk(self):
        adapter = _make_adapter()
        risk = _make_risk()
        risk.config.max_positions = 0
        contract = _make_contract()
        adapter.find_options.return_value = [contract]
        adapter.enrich_contract.return_value = contract
        adapter.get_positions.return_value = {}

        strat = MomentumOptionsStrategy(adapter, risk, roc_period=14, threshold=5.0,
                                         target_dte=37, profit_target_pct=50.0,
                                         stop_loss_pct=30.0, position_size_pct=3.0)
        with patch.object(strat, '_get_momentum_signal', return_value=1):
            actions = strat.evaluate("BTC")
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
