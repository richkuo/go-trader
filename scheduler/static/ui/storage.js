(function () {
  "use strict";

  const CHART_PREFERENCES_KEY = "goTraderChartPreferences";

  function createGuardedStorage(getStorage) {
    function target() {
      const storage = getStorage();
      if (!storage) throw new Error("storage unavailable");
      return storage;
    }
    return {
      read: function (key) {
        try {
          const value = target().getItem(key);
          return { ok: true, value: value === null || value === undefined ? null : String(value) };
        } catch (_err) {
          return { ok: false };
        }
      },
      write: function (key, value) {
        try {
          target().setItem(key, value);
          return { ok: true };
        } catch (_err) {
          return { ok: false };
        }
      },
      remove: function (key) {
        try {
          target().removeItem(key);
          return { ok: true };
        } catch (_err) {
          return { ok: false };
        }
      },
    };
  }

  function createPreferenceAccessor(storage, key) {
    return {
      read: function () {
        return storage.read(key);
      },
      write: function (value) {
        return storage.write(key, value);
      },
    };
  }

  function chartPreferences(getStorage) {
    return createPreferenceAccessor(createGuardedStorage(getStorage), CHART_PREFERENCES_KEY);
  }

  const api = {
    CHART_PREFERENCES_KEY: CHART_PREFERENCES_KEY,
    createGuardedStorage: createGuardedStorage,
    createPreferenceAccessor: createPreferenceAccessor,
    chartPreferences: chartPreferences,
  };

  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.GoTraderStorage = api;
})();
