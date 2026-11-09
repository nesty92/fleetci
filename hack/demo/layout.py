"""The video's layout, shared by record.py (the terminals' sizes) and
render.py (where they go). 1920x1080; a header, the admin and forge panes
across the top, the four machines' logs below, the captions at the bottom."""

W, H = 1920, 1080
FONT_PX, CELL_W, LINE_H = 14, 8.43, 17  # DejaVu Sans Mono at 14 px
HEADER, FOOTER, GAP, TITLE, PAD = 64, 84, 12, 26, 8

TOP_H = TITLE + 2 * PAD + 13 * LINE_H
LOG_H = (H - HEADER - FOOTER - TOP_H - 3 * GAP) // 2


def _cols(w):
    return int((w - 2 * PAD) // CELL_W)


def _rows(h):
    return int((h - TITLE - 2 * PAD) // LINE_H)


ADMIN_W = 1120
FORGE_W = W - 3 * GAP - ADMIN_W
LOG_W = (W - 3 * GAP) // 2

# name: (x, y, w, h, title)
PANES = {
    'admin': (GAP, HEADER, ADMIN_W, TOP_H, 'ctl · admin'),
    'forge': (2 * GAP + ADMIN_W, HEADER, FORGE_W, TOP_H, 'the forges'),
    'ctl': (GAP, HEADER + TOP_H + GAP, LOG_W, LOG_H, 'ctl · controller · core'),
    'eu': (2 * GAP + LOG_W, HEADER + TOP_H + GAP, LOG_W, LOG_H, 'eu · worker · +20 ms'),
    'us': (GAP, HEADER + TOP_H + 2 * GAP + LOG_H, LOG_W, LOG_H, 'us · worker · +80 ms'),
    'ap': (2 * GAP + LOG_W, HEADER + TOP_H + 2 * GAP + LOG_H, LOG_W, LOG_H, 'ap · worker · +160 ms'),
}


def size(name):
    _, _, w, h, _ = PANES[name]
    return _cols(w), _rows(h)
