#!/usr/bin/env python
# License: GPLv3 Copyright: 2020, Kovid Goyal <kovid at kovidgoyal.net>

import base64
import sys
from functools import partial
from typing import TYPE_CHECKING, Any, Dict, List, Optional, Set, Union

from kitty.fast_data_types import (
    KeyEvent as WindowSystemKeyEvent, add_timer, get_boss, remove_timer, get_options
)
from kitty.key_encoding import decode_key_event_as_window_system_key
from kitty.options.utils import parse_send_text_bytes

from .base import (
    MATCH_TAB_OPTION, MATCH_WINDOW_OPTION, ArgsType, Boss, CmdGenerator,
    MatchError, PayloadGetType, PayloadType, RCOptions, RemoteCommand,
    ResponseType, Window
)

if TYPE_CHECKING:
    from kitty.cli_stub import SendTextRCOptions as CLIOptions


class Session:
    id: str
    window_ids: Set[int]
    timer_id: int = 0

    def __init__(self, id: str):
        self.id = id
        self.window_ids = set()


sessions_map: Dict[str, Session] = {}


def clear_unfocused_cursors(sid: str, *a: Any) -> None:
    s = sessions_map.pop(sid, None)
    if s is not None:
        boss = get_boss()
        for wid in s.window_ids:
            qw = boss.window_id_map.get(wid)
            if qw is not None:
                qw.screen.render_unfocused_cursor = 0


class SendText(RemoteCommand):
    '''
    data+: The data being sent. Can be either: text: followed by text or base64: followed by standard base64 encoded bytes
    match: A string indicating the window to send text to
    match_tab: A string indicating the tab to send text to
    all: A boolean indicating all windows should be matched.
    exclude_active: A boolean that prevents sending text to the active window
    session_id: A string that identifies a "broadcast session"
    '''
    short_desc = 'Send arbitrary text to specified windows'
    desc = (
        'Send arbitrary text to specified windows. The text follows Python'
        ' escaping rules. So you can use escapes like :italic:`\\x1b` to send control codes'
        ' and :italic:`\\u21fa` to send unicode characters. If you use the :option:`kitty @ send-text --match` option'
        ' the text will be sent to all matched windows. By default, text is sent to'
        ' only the currently active window.'
    )
    options_spec = MATCH_WINDOW_OPTION + '\n\n' + MATCH_TAB_OPTION.replace('--match -m', '--match-tab -t') + '''\n
--all
type=bool-set
Match all windows.


--stdin
type=bool-set
Read the text to be sent from :italic:`stdin`. Note that in this case the text is sent as is,
not interpreted for escapes. If stdin is a terminal, you can press Ctrl-D to end reading.


--from-file
Path to a file whose contents you wish to send. Note that in this case the file contents
are sent as is, not interpreted for escapes.


--exclude-active
type=bool-set
Do not send text to the active window, even if it is one of the matched windows.
'''
    no_response = True
    argspec = '[TEXT TO SEND]'

    def message_to_kitty(self, global_opts: RCOptions, opts: 'CLIOptions', args: ArgsType) -> PayloadType:
        limit = 1024
        ret = {'match': opts.match, 'data': '', 'match_tab': opts.match_tab, 'all': opts.all, 'exclude_active': opts.exclude_active}

        def pipe() -> CmdGenerator:
            if sys.stdin.isatty():
                ret['exclude_active'] = True
                keep_going = True
                from kitty.utils import TTYIO
                with TTYIO(read_with_timeout=False) as tty:
                    while keep_going:
                        if not tty.wait_till_read_available():
                            break
                        data = tty.read(limit)
                        if not data:
                            break
                        decoded_data = data.decode('utf-8')
                        if '\x04' in decoded_data:
                            decoded_data = decoded_data[:decoded_data.index('\x04')]
                            keep_going = False
                        ret['data'] = f'text:{decoded_data}'
                        yield ret
            else:
                while True:
                    data = sys.stdin.buffer.read(limit)
                    if not data:
                        break
                    ret['data'] = f'base64:{base64.standard_b64encode(data).decode("ascii")}'
                    yield ret

        def chunks(text: str) -> CmdGenerator:
            data = parse_send_text_bytes(text).decode('utf-8')
            while data:
                ret['data'] = f'text:{data[:limit]}'
                yield ret
                data = data[limit:]

        def file_pipe(path: str) -> CmdGenerator:
            with open(path, 'rb') as f:
                while True:
                    data = f.read(limit)
                    if not data:
                        break
                    ret['data'] = f'base64:{base64.standard_b64encode(data).decode("ascii")}'
                    yield ret

        sources = []
        if opts.stdin:
            sources.append(pipe())

        if opts.from_file:
            sources.append(file_pipe(opts.from_file))

        text = ' '.join(args)
        sources.append(chunks(text))

        def chain() -> CmdGenerator:
            for src in sources:
                yield from src
        return chain()

    def response_from_kitty(self, boss: Boss, window: Optional[Window], payload_get: PayloadGetType) -> ResponseType:
        if payload_get('all'):
            windows: List[Optional[Window]] = list(boss.all_windows)
        else:
            windows = [boss.active_window]
            match = payload_get('match')
            if match:
                windows = list(boss.match_windows(match))
            mt = payload_get('match_tab')
            if mt:
                windows = []
                tabs = tuple(boss.match_tabs(mt))
                if not tabs:
                    raise MatchError(payload_get('match_tab'), 'tabs')
                for tab in tabs:
                    windows += tuple(tab)
        pdata: str = payload_get('data')
        encoding, _, q = pdata.partition(':')
        session = ''
        if encoding == 'text':
            data: Union[bytes, WindowSystemKeyEvent] = q.encode('utf-8')
        elif encoding == 'base64':
            data = base64.standard_b64decode(q)
        elif encoding == 'kitty-key':
            bdata = base64.standard_b64decode(q)
            candidate = decode_key_event_as_window_system_key(bdata.decode('ascii'))
            if candidate is None:
                raise ValueError(f'Could not decode window system key: {q}')
            data = candidate
        elif encoding == 'session':
            session = q
        else:
            raise TypeError(f'Invalid encoding for send-text data: {encoding}')
        exclude_active = payload_get('exclude_active')
        actual_windows = (w for w in windows if w is not None and (not exclude_active or w is not boss.active_window))
        sid = payload_get('session_id', '')

        def create_or_update_session() -> Session:
            s = sessions_map.setdefault(sid, Session(sid))
            if s.timer_id:
                remove_timer(s.timer_id)
                s.timer_id = 0
            return s
        blink_time = 15 if not get_options().cursor_stop_blinking_after else max(3, get_options().cursor_stop_blinking_after)

        if session == 'end':
            s = create_or_update_session()
            for w in actual_windows:
                w.screen.render_unfocused_cursor = 0
                s.window_ids.discard(w.id)
            clear_unfocused_cursors(sid)
        elif session == 'start':
            s = create_or_update_session()
            if window is not None:
                window.actions_on_close.append(partial(clear_unfocused_cursors, sid))
            s.timer_id = add_timer(partial(clear_unfocused_cursors, sid), blink_time, False)
            for w in actual_windows:
                w.screen.render_unfocused_cursor = 1
                s.window_ids.add(w.id)
        else:
            if sid:
                s = create_or_update_session()
                s.timer_id = add_timer(partial(clear_unfocused_cursors, sid), blink_time, False)
            for w in actual_windows:
                if sid:
                    w.screen.render_unfocused_cursor = 1
                    s.window_ids.add(w.id)
                if isinstance(data, WindowSystemKeyEvent):
                    kdata = w.encoded_key(data)
                    if kdata:
                        w.write_to_child(kdata)
                else:
                    w.write_to_child(data)
        return None


send_text = SendText()
