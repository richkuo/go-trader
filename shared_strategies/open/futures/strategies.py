import importlib.util
import json
import os
import sys
from typing import Dict, List, Optional
import pandas as pd
_TOOLS_DIR = os.path.join(os.path.dirname(__file__), '..', '..', '..', 'shared_tools')
if _TOOLS_DIR not in sys.path:
    sys.path.insert(0, _TOOLS_DIR)
from strategy_composition import strip_unsupported_position_context

def _load_registry_module():
    registry_path = os.path.join(os.path.dirname(__file__), '..', 'registry.py')
    spec = importlib.util.spec_from_file_location('_strategy_registry_futures', registry_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod
_registry = _load_registry_module()
STRATEGY_REGISTRY: Dict[str, dict] = _registry.build_registry('futures', include_hidden=True)
DISCOVERY_STRATEGY_REGISTRY: Dict[str, dict] = _registry.build_registry('futures')

def get_strategy(name: str) -> dict:
    if name not in STRATEGY_REGISTRY:
        raise ValueError(f'Unknown strategy: {name}. Available: {list(STRATEGY_REGISTRY.keys())}')
    return STRATEGY_REGISTRY[name]

def list_strategies() -> List[str]:
    return list(DISCOVERY_STRATEGY_REGISTRY.keys())

def apply_strategy(name: str, df: pd.DataFrame, params: Optional[dict]=None) -> pd.DataFrame:
    strat = get_strategy(name)
    p = {**strat['default_params'], **(params or {})}
    p = strip_unsupported_position_context(strat['fn'], p)
    return strat['fn'](df, **p)

def validate_params(name: str, params: Optional[dict]=None, default_params: Optional[dict]=None) -> None:
    if default_params is None:
        default_params = get_strategy(name)['default_params']
    _registry.validate_params(name, params or {}, default_params)

def validate_param_value(name: str, param: str, value) -> None:
    _registry.validate_param_value(name, param, value)
if __name__ == '__main__':
    if '--list-json' in sys.argv:
        print(json.dumps([{'id': name, 'description': DISCOVERY_STRATEGY_REGISTRY[name]['description']} for name in list_strategies()]))
    else:
        print(f'Registered strategies: {list_strategies()}')
        for name in list_strategies():
            s = DISCOVERY_STRATEGY_REGISTRY[name]
            print(f"  {name}: {s['description']}")
            print(f"    Defaults: {s['default_params']}")
