import struct

FRAME_MAGIC = 0x184D2204
SKIPPABLE_MASK = 0xFFFFFFF0
SKIPPABLE_MAGIC = 0x184D2A50
BLOCK_MAX = {4: 64 << 10, 5: 256 << 10, 6: 1 << 20, 7: 4 << 20}
P1, P2, P3, P4, P5 = 2654435761, 2246822519, 3266489917, 668265263, 374761393
M32 = 0xFFFFFFFF


class LZ4FrameError(ValueError):
    pass


def _rotl(x: int, r: int) -> int:
    return ((x << r) | (x >> (32 - r))) & M32


def xxh32(data, seed: int = 0) -> int:
    n = len(data)
    i = 0
    if n >= 16:
        v1 = (seed + P1 + P2) & M32
        v2 = (seed + P2) & M32
        v3 = seed & M32
        v4 = (seed - P1) & M32
        limit = n - 16
        unpack = struct.Struct("<4I").unpack_from
        while i <= limit:
            a, b, c, d = unpack(data, i)
            v1 = (_rotl((v1 + a * P2) & M32, 13) * P1) & M32
            v2 = (_rotl((v2 + b * P2) & M32, 13) * P1) & M32
            v3 = (_rotl((v3 + c * P2) & M32, 13) * P1) & M32
            v4 = (_rotl((v4 + d * P2) & M32, 13) * P1) & M32
            i += 16
        h = (_rotl(v1, 1) + _rotl(v2, 7) + _rotl(v3, 12) + _rotl(v4, 18)) & M32
    else:
        h = (seed + P5) & M32
    h = (h + n) & M32
    while i + 4 <= n:
        (k,) = struct.unpack_from("<I", data, i)
        h = (_rotl((h + k * P3) & M32, 17) * P4) & M32
        i += 4
    while i < n:
        h = (_rotl((h + data[i] * P5) & M32, 11) * P1) & M32
        i += 1
    h ^= h >> 15
    h = (h * P2) & M32
    h ^= h >> 13
    h = (h * P3) & M32
    h ^= h >> 16
    return h


def _decode_block(src, start: int, end: int, out: bytearray, window_floor: int) -> None:
    i = start
    while True:
        if i >= end:
            raise LZ4FrameError("block ends inside a sequence")
        token = src[i]
        i += 1
        lit = token >> 4
        if lit == 15:
            while True:
                if i >= end:
                    raise LZ4FrameError("block ends inside a literal length")
                b = src[i]
                i += 1
                lit += b
                if b != 255:
                    break
        if i + lit > end:
            raise LZ4FrameError("literal run passes the block end")
        out += src[i:i + lit]
        i += lit
        if i == end:
            return
        if i + 2 > end:
            raise LZ4FrameError("block ends inside a match offset")
        offset = src[i] | (src[i + 1] << 8)
        i += 2
        if offset == 0 or offset > len(out) - window_floor:
            raise LZ4FrameError(f"match offset {offset} is outside the decoded window")
        mlen = token & 15
        if mlen == 15:
            while True:
                if i >= end:
                    raise LZ4FrameError("block ends inside a match length")
                b = src[i]
                i += 1
                mlen += b
                if b != 255:
                    break
        mlen += 4
        pos = len(out) - offset
        if offset >= mlen:
            out += out[pos:pos + mlen]
        else:
            chunk = out[pos:]
            while mlen > 0:
                take = chunk[:mlen]
                out += take
                mlen -= len(take)
                chunk = chunk + take


def decompress(src) -> bytes:
    src = memoryview(bytes(src)).cast("B")
    n = len(src)
    out = bytearray()
    i = 0
    frames = 0
    while i < n:
        if i + 4 > n:
            raise LZ4FrameError("trailing bytes after the last frame")
        (magic,) = struct.unpack_from("<I", src, i)
        if (magic & SKIPPABLE_MASK) == SKIPPABLE_MAGIC:
            if i + 8 > n:
                raise LZ4FrameError("truncated skippable frame header")
            (size,) = struct.unpack_from("<I", src, i + 4)
            i += 8 + size
            if i > n:
                raise LZ4FrameError("truncated skippable frame")
            continue
        if magic != FRAME_MAGIC:
            raise LZ4FrameError(f"frame magic {magic:#x} is not an LZ4 frame")
        i += 4
        if i + 2 > n:
            raise LZ4FrameError("truncated frame descriptor")
        flg, bd = src[i], src[i + 1]
        if flg >> 6 != 1:
            raise LZ4FrameError("unsupported LZ4 frame version")
        if flg & 0x02 or bd & 0x8F:
            raise LZ4FrameError("reserved frame descriptor bits are set")
        independent = bool(flg & 0x20)
        block_checksum = bool(flg & 0x10)
        content_size = bool(flg & 0x08)
        content_checksum = bool(flg & 0x04)
        if flg & 0x01:
            raise LZ4FrameError("dictionary frames are not supported")
        block_max = BLOCK_MAX.get((bd >> 4) & 7)
        if block_max is None:
            raise LZ4FrameError("invalid block maximum size")
        desc_start = i
        i += 2
        declared = None
        if content_size:
            if i + 8 > n:
                raise LZ4FrameError("truncated content size")
            (declared,) = struct.unpack_from("<Q", src, i)
            i += 8
        if i >= n:
            raise LZ4FrameError("truncated header checksum")
        if (xxh32(bytes(src[desc_start:i])) >> 8) & 0xFF != src[i]:
            raise LZ4FrameError("frame header checksum mismatch")
        i += 1
        frame_start = len(out)
        while True:
            if i + 4 > n:
                raise LZ4FrameError("truncated block size")
            (size,) = struct.unpack_from("<I", src, i)
            i += 4
            if size == 0:
                break
            raw = bool(size & 0x80000000)
            size &= 0x7FFFFFFF
            if size > block_max or i + size > n:
                raise LZ4FrameError("block size is invalid or passes the end of input")
            block = src[i:i + size]
            if block_checksum:
                if i + size + 4 > n:
                    raise LZ4FrameError("truncated block checksum")
                (want,) = struct.unpack_from("<I", src, i + size)
                if xxh32(bytes(block)) != want:
                    raise LZ4FrameError("block checksum mismatch")
            before = len(out)
            if raw:
                out += block
            else:
                _decode_block(src, i, i + size, out, before if independent else frame_start)
            if len(out) - before > block_max:
                raise LZ4FrameError("block decodes past the block maximum size")
            i += size + (4 if block_checksum else 0)
        if content_checksum:
            if i + 4 > n:
                raise LZ4FrameError("truncated content checksum")
            (want,) = struct.unpack_from("<I", src, i)
            if xxh32(bytes(out[frame_start:])) != want:
                raise LZ4FrameError("content checksum mismatch")
            i += 4
        if declared is not None and declared != len(out) - frame_start:
            raise LZ4FrameError("decoded size differs from the declared content size")
        frames += 1
    if frames == 0:
        raise LZ4FrameError("input holds no LZ4 frame")
    return bytes(out)
