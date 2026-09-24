
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


