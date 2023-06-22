#!/usr/bin/env python3
# vim:fileencoding=utf-8

import os
from functools import lru_cache
from typing import List


def to_linear(a: float) -> float:
    if a <= 0.04045:
        return a / 12.92
    else:
        return float(pow((a + 0.055) / 1.055, 2.4))


@lru_cache
def generate_srgb_lut(line_prefix: str = '    ') -> List[str]:
    values: List[str] = []
    lines: List[str] = []

    for i in range(256):
        values.append('{:1.5f}f'.format(to_linear(i / 255.0)))

    for i in range(16):
        lines.append(line_prefix + ', '.join(values[i * 16:(i + 1) * 16]) + ',')

    lines[-1] = lines[-1].rstrip(',')
    return lines


def generate_srgb_gamma(declaration: str = 'static const GLfloat srgb_lut[256] = {', close: str = '};') -> str:
    lines: List[str] = []
    a = lines.append

    a('// Generated by gen-srgb-lut.py DO NOT edit')
    a('')
    a(declaration)
    lines += generate_srgb_lut()
    a(close)

    return "\n".join(lines)


def main() -> None:
    c = generate_srgb_gamma()
    with open(os.path.join('kitty', 'srgb_gamma.h'), 'w') as f:
        f.write(f'{c}\n')
    g = generate_srgb_gamma('const float gamma_lut[256] = float[256](', ');')
    with open(os.path.join('kitty', 'srgb_gamma.glsl'), 'w') as f:
        f.write(f'{g}\n')


if __name__ == '__main__':
    main()
