/* Block-shuffle Kruskal-Wallis counts for the #1940 study.
   Fisher-Yates uses xorshift64* seeded by splitmix64.
   count_block_exceedances walks blocks and is the reference.
   count_block_exceedances_fast precomputes contributions for one contiguous run.
   Average ranks are stored in half-rank units so .0 and .5 stay exact. */

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

enum { PERM_MAX_K = 16 };

static uint64_t splitmix64(uint64_t *x) {
    uint64_t z = (*x += 0x9E3779B97F4A7C15ULL);
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ULL;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBULL;
    return z ^ (z >> 31);
}

static uint64_t rng_next(uint64_t *state) {
    uint64_t x = *state;
    x ^= x >> 12;
    x ^= x << 25;
    x ^= x >> 27;
    *state = x;
    return x * 0x2545F4914F6CDD1DULL;
}

static void fisher_yates(uint64_t *state, int32_t *order, int nb) {
    int i;
    for (i = 0; i < nb; i++) {
        order[i] = i;
    }
    for (i = nb - 1; i > 0; i--) {
        int j = (int)(rng_next(state) % (uint64_t)(i + 1));
        int32_t tmp = order[i];
        order[i] = order[j];
        order[j] = tmp;
    }
}

static int block_count(int n, int block_len) {
    return (n + block_len - 1) / block_len;
}

static int block_origin(int block_index, int block_len) {
    return block_index * block_len;
}

static int block_length(int block_index, int n_blocks, int n, int block_len) {
    int len = n - block_origin(block_index, block_len);
    if (block_index < n_blocks - 1 || len > block_len) {
        return block_len;
    }
    return len;
}

static int64_t rank_units(double rank) {
    return (int64_t)(rank * 2.0 + (rank >= 0.0 ? 0.5 : -0.5));
}

static double h_from_unit_sums(const int64_t *sum, const int *cnt, int n, int k, double inv_tie) {
    double acc = 0.0, h;
    int c, nonempty = 0;
    if (n < 2) {
        return 0.0;
    }
    for (c = 0; c < k; c++) {
        if (cnt[c] > 0) {
            nonempty += 1;
            acc += ((double)sum[c] * (double)sum[c]) / (double)cnt[c];
        }
    }
    if (nonempty < 2) {
        return 0.0;
    }
    acc *= 0.25;
    h = (12.0 / ((double)n * ((double)n + 1.0))) * acc - 3.0 * ((double)n + 1.0);
    if (inv_tie > 0.0) {
        h *= inv_tie;
    }
    return h;
}

static void emit(int64_t ge, int64_t executed, double h_obs,
                 int64_t *ge_out, int64_t *executed_out, double *h_obs_out) {
    if (ge_out) *ge_out = ge;
    if (executed_out) *executed_out = executed;
    if (h_obs_out) *h_obs_out = h_obs;
}

double h_for_block_order(
    const int32_t *labels,
    const double *ranks,
    int n,
    int k,
    int block_len,
    const int32_t *order,
    int n_blocks,
    double inv_tie
) {
    int64_t sum[PERM_MAX_K];
    int cnt[PERM_MAX_K];
    int c, d, t, offset = 0;
    if (k < 1 || k > PERM_MAX_K || n_blocks < 1 || block_len < 1) {
        return 0.0;
    }
    for (c = 0; c < k; c++) {
        sum[c] = 0;
        cnt[c] = 0;
    }
    for (d = 0; d < n_blocks; d++) {
        int s = order[d];
        int origin = block_origin(s, block_len);
        int len = block_length(s, n_blocks, n, block_len);
        for (t = 0; t < len; t++) {
            int lab = labels[origin + t];
            if (lab < 0 || lab >= k) {
                return 0.0;
            }
            sum[lab] += rank_units(ranks[offset + t]);
            cnt[lab] += 1;
        }
        offset += len;
    }
    if (offset != n) {
        return 0.0;
    }
    return h_from_unit_sums(sum, cnt, n, k, inv_tie);
}

void count_block_exceedances(
    const int32_t *labels,
    const double *ranks,
    int n,
    int k,
    int block_len,
    double h_obs,
    double inv_tie,
    uint64_t seed,
    int64_t n_perm,
    int64_t stop_ge,
    int64_t *ge_out,
    int64_t *executed_out,
    double *h_obs_out
) {
    int n_blocks, i, c;
    int cnt[PERM_MAX_K];
    int32_t *order = NULL;
    int *origin = NULL;
    int *blen = NULL;
    int64_t *rank_u = NULL;
    uint64_t sm, state;
    int64_t step, ge;
    double observed, scale;
    if (n <= 1 || k < 2 || k > PERM_MAX_K || block_len < 1 || n_perm < 1) {
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    n_blocks = block_count(n, block_len);
    order = (int32_t *)malloc((size_t)n_blocks * sizeof(int32_t));
    origin = (int *)malloc((size_t)n_blocks * sizeof(int));
    blen = (int *)malloc((size_t)n_blocks * sizeof(int));
    rank_u = (int64_t *)malloc((size_t)n * sizeof(int64_t));
    if (!order || !origin || !blen || !rank_u) {
        free(order); free(origin); free(blen); free(rank_u);
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    for (c = 0; c < k; c++) cnt[c] = 0;
    for (i = 0; i < n; i++) {
        rank_u[i] = rank_units(ranks[i]);
        cnt[labels[i]] += 1;
    }
    for (i = 0; i < n_blocks; i++) {
        order[i] = i;
        origin[i] = block_origin(i, block_len);
        blen[i] = block_length(i, n_blocks, n, block_len);
    }
    observed = h_obs >= 0.0 ? h_obs : h_for_block_order(labels, ranks, n, k, block_len, order, n_blocks, inv_tie);
    scale = 12.0 / ((double)n * ((double)n + 1.0));
    sm = seed;
    state = splitmix64(&sm);
    ge = 0;
    for (step = 0; step < n_perm; step++) {
        int64_t sum[PERM_MAX_K];
        int d, cursor = 0, nonempty = 0;
        double acc = 0.0, h;
        fisher_yates(&state, order, n_blocks);
        for (c = 0; c < k; c++) sum[c] = 0;
        for (d = 0; d < n_blocks; d++) {
            int s = order[d];
            int off = origin[s];
            int len = blen[s];
            int t;
            for (t = 0; t < len; t++) {
                sum[labels[off + t]] += rank_u[cursor + t];
            }
            cursor += len;
        }
        for (c = 0; c < k; c++) {
            if (cnt[c] > 0) {
                nonempty += 1;
                acc += ((double)sum[c] * (double)sum[c]) / (double)cnt[c];
            }
        }
        h = nonempty < 2 ? 0.0 : (scale * (acc * 0.25) - 3.0 * ((double)n + 1.0)) * (inv_tie > 0.0 ? inv_tie : 1.0);
        if (h >= observed) {
            ge += 1;
            if (stop_ge > 0 && ge >= stop_ge) {
                step += 1;
                break;
            }
        }
    }
    free(order); free(origin); free(blen); free(rank_u);
    emit(ge, step, observed, ge_out, executed_out, h_obs_out);
}

void count_block_exceedances_fast(
    const int32_t *labels,
    const double *ranks,
    int n,
    int k,
    int block_len,
    double h_obs,
    double inv_tie,
    uint64_t seed,
    int64_t n_perm,
    int64_t stop_ge,
    int64_t *ge_out,
    int64_t *executed_out,
    double *h_obs_out
) {
    int n_blocks, tail, full_slots, cursor_n, s, c, t;
    int cnt[PERM_MAX_K];
    int32_t *order = NULL;
    int64_t *contrib = NULL;
    uint64_t sm, state;
    int64_t step, ge;
    double observed;
    size_t cell_n;
    if (n <= 1 || k < 2 || k > PERM_MAX_K || block_len < 1 || n_perm < 1) {
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    n_blocks = block_count(n, block_len);
    tail = n % block_len;
    full_slots = n / block_len;
    cursor_n = tail ? (full_slots + 1) * 2 : (full_slots + 1);
    cell_n = (size_t)n_blocks * (size_t)cursor_n * (size_t)k;
    if (cell_n > (size_t)4 * 1024 * 1024) {
        count_block_exceedances(labels, ranks, n, k, block_len, h_obs, inv_tie, seed,
                                n_perm, stop_ge, ge_out, executed_out, h_obs_out);
        return;
    }
    order = (int32_t *)malloc((size_t)n_blocks * sizeof(int32_t));
    contrib = (int64_t *)calloc(cell_n, sizeof(int64_t));
    if (!order || !contrib) {
        free(order);
        free(contrib);
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    for (c = 0; c < k; c++) {
        cnt[c] = 0;
    }
    for (s = 0; s < n; s++) {
        cnt[labels[s]] += 1;
    }
    for (s = 0; s < n_blocks; s++) {
        int origin = block_origin(s, block_len);
        int len = block_length(s, n_blocks, n, block_len);
        int full_before, short_flag, nflags = tail ? 2 : 1;
        for (full_before = 0; full_before <= full_slots; full_before++) {
            for (short_flag = 0; short_flag < nflags; short_flag++) {
                int cursor = full_before * block_len + (short_flag ? tail : 0);
                int index = tail ? full_before * 2 + short_flag : full_before;
                int64_t *cell;
                if (index >= cursor_n || cursor + len > n) {
                    continue;
                }
                cell = contrib + (((size_t)s * (size_t)cursor_n + (size_t)index) * (size_t)k);
                for (t = 0; t < len; t++) {
                    cell[labels[origin + t]] += rank_units(ranks[cursor + t]);
                }
            }
        }
    }
    for (s = 0; s < n_blocks; s++) {
        order[s] = s;
    }
    observed = h_obs >= 0.0 ? h_obs : h_for_block_order(labels, ranks, n, k, block_len, order, n_blocks, inv_tie);
    sm = seed;
    state = splitmix64(&sm);
    ge = 0;
    for (step = 0; step < n_perm; step++) {
        int64_t sum[PERM_MAX_K];
        int full_before = 0, short_placed = 0, d;
        double h;
        fisher_yates(&state, order, n_blocks);
        for (c = 0; c < k; c++) {
            sum[c] = 0;
        }
        for (d = 0; d < n_blocks; d++) {
            int src = order[d];
            int is_short = tail && src == n_blocks - 1;
            int index = tail ? full_before * 2 + short_placed : full_before;
            int64_t *cell = contrib + (((size_t)src * (size_t)cursor_n + (size_t)index) * (size_t)k);
            for (c = 0; c < k; c++) {
                sum[c] += cell[c];
            }
            if (is_short) {
                short_placed = 1;
            } else {
                full_before += 1;
            }
        }
        h = h_from_unit_sums(sum, cnt, n, k, inv_tie);
        if (h >= observed) {
            ge += 1;
            if (stop_ge > 0 && ge >= stop_ge) {
                step += 1;
                break;
            }
        }
    }
    free(order);
    free(contrib);
    emit(ge, step, observed, ge_out, executed_out, h_obs_out);
}

/* Blocks stay inside each run. One RNG stream walks runs in order. */
void count_block_exceedances_runs(
    const int32_t *labels,
    const double *ranks,
    int n,
    int k,
    const int32_t *run_lo,
    const int32_t *run_hi,
    int n_runs,
    int block_len,
    double h_obs,
    double inv_tie,
    uint64_t seed,
    int64_t n_perm,
    int64_t stop_ge,
    int64_t *ge_out,
    int64_t *executed_out,
    double *h_obs_out
) {
    int r, i, c;
    int *run_blocks = NULL;
    int32_t *order = NULL;
    int max_blocks = 0;
    int cnt[PERM_MAX_K];
    uint64_t sm, state;
    int64_t step, ge;
    double observed;
    if (n_runs == 1 && run_lo && run_hi && run_lo[0] == 0 && run_hi[0] == n) {
        count_block_exceedances_fast(labels, ranks, n, k, block_len, h_obs, inv_tie, seed,
                                     n_perm, stop_ge, ge_out, executed_out, h_obs_out);
        return;
    }
    if (n <= 1 || k < 2 || k > PERM_MAX_K || n_runs < 1 || block_len < 1 || n_perm < 1) {
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    run_blocks = (int *)malloc((size_t)n_runs * sizeof(int));
    if (!run_blocks) {
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    for (r = 0; r < n_runs; r++) {
        int len = run_hi[r] - run_lo[r];
        run_blocks[r] = len > 0 ? block_count(len, block_len) : 0;
        if (run_blocks[r] > max_blocks) {
            max_blocks = run_blocks[r];
        }
    }
    order = (int32_t *)malloc((size_t)(max_blocks > 0 ? max_blocks : 1) * sizeof(int32_t));
    if (!order) {
        free(run_blocks);
        emit(0, 0, 0.0, ge_out, executed_out, h_obs_out);
        return;
    }
    for (c = 0; c < k; c++) {
        cnt[c] = 0;
    }
    for (i = 0; i < n; i++) {
        cnt[labels[i]] += 1;
    }
    {
        int64_t sum[PERM_MAX_K];
        int offset = 0;
        for (c = 0; c < k; c++) sum[c] = 0;
        for (i = 0; i < n; i++) sum[labels[i]] += rank_units(ranks[i]);
        observed = h_obs >= 0.0 ? h_obs : h_from_unit_sums(sum, cnt, n, k, inv_tie);
        (void)offset;
    }
    sm = seed;
    state = splitmix64(&sm);
    ge = 0;
    for (step = 0; step < n_perm; step++) {
        int64_t sum[PERM_MAX_K];
        double h;
        for (c = 0; c < k; c++) sum[c] = 0;
        for (r = 0; r < n_runs; r++) {
            int nb = run_blocks[r];
            int base = run_lo[r];
            int len = run_hi[r] - run_lo[r];
            int d, cursor = 0;
            if (nb <= 0) continue;
            fisher_yates(&state, order, nb);
            for (d = 0; d < nb; d++) {
                int s = order[d];
                int origin = s * block_len;
                int bl = len - origin;
                int t;
                if (bl > block_len) bl = block_len;
                if (s < nb - 1) bl = block_len;
                for (t = 0; t < bl; t++) {
                    sum[labels[base + origin + t]] += rank_units(ranks[base + cursor + t]);
                }
                cursor += bl;
            }
        }
        h = h_from_unit_sums(sum, cnt, n, k, inv_tie);
        if (h >= observed) {
            ge += 1;
            if (stop_ge > 0 && ge >= stop_ge) {
                step += 1;
                break;
            }
        }
    }
    free(order);
    free(run_blocks);
    emit(ge, step, observed, ge_out, executed_out, h_obs_out);
}
