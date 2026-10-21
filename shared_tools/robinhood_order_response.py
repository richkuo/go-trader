_REFUSED_STATES = ("rejected", "failed")
_REFUSAL_KEYS = ("detail", "non_field_errors")


def robinhood_order_response_error(response):
    if not isinstance(response, dict):
        return f"unexpected Robinhood order response type {type(response).__name__}"
    if not response:
        return "no Robinhood order response after the submit retries; the order outcome is unknown, check the account"
    for key in _REFUSAL_KEYS:
        if response.get(key):
            return f"Robinhood refused the order: {key}={response[key]!r}"
    order_id = response.get("id")
    if not isinstance(order_id, str) or not order_id.strip():
        return "Robinhood order response has no order id"
    state = response.get("state")
    if isinstance(state, str) and state.strip().lower() in _REFUSED_STATES:
        return f"Robinhood order {order_id} is {state.strip().lower()}"
    return None
