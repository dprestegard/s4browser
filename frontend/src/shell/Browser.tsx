import {CSSProperties, KeyboardEvent, MouseEvent, useEffect, useLayoutEffect, useRef, useState} from 'react';
import {defaultRangeExtractor, useVirtualizer} from '@tanstack/react-virtual';
import {CloseListing, ListingKeys, ListingLocate, ListingSummary, ListingWindow as FetchWindow, OpenListing} from '../../wailsjs/go/app/App';
import {EventsOn} from '../../wailsjs/runtime/runtime';
import {Icon} from '../ui/Icon';
import {useDismiss} from '../ui/overlay';
import {formatDate, formatSize, formatStorageClass, plural} from '../ui/format';
import {ListingWindow, Location, noSelection, Row, SelectionInfo, SelectionRequest, Snapshot, Sort, SortField} from '../types';
import {Density} from '../settings';

// The table is virtualized: only the rows in view (plus a little overscan) exist
// in the page, and only those rows are fetched from Go, in blocks. Go owns the
// listing and its sort; the page never holds more than a few blocks of rows.
const BLOCK = 100;
const OVERSCAN = 12;
const HEAD_H = 28;

// A selection is a list of sorted, non-touching, inclusive index ranges in the
// current sort order, so Ctrl+A over 100,000 rows is one pair of numbers. Go
// totals the ranges (ListingSummary). Ranges are only valid for one sort order
// and one version of the listing, so they are dropped when either changes.
type Range = [number, number];
const noRanges: Range[] = [];

function inRanges(ranges: Range[], i: number): boolean {
    let lo = 0, hi = ranges.length - 1;
    while (lo <= hi) {
        const mid = (lo + hi) >> 1;
        if (i < ranges[mid][0]) hi = mid - 1;
        else if (i > ranges[mid][1]) lo = mid + 1;
        else return true;
    }
    return false;
}

function addRange(ranges: Range[], a: number, b: number): Range[] {
    const out: Range[] = [];
    let from = Math.min(a, b);
    let to = Math.max(a, b);
    let placed = false;
    for (const r of ranges) {
        if (r[1] < from - 1) out.push(r);
        else if (r[0] > to + 1) {
            if (!placed) {
                out.push([from, to]);
                placed = true;
            }
            out.push(r);
        } else {
            from = Math.min(from, r[0]);
            to = Math.max(to, r[1]);
        }
    }
    if (!placed) out.push([from, to]);
    return out;
}

function removeIndex(ranges: Range[], i: number): Range[] {
    const out: Range[] = [];
    for (const r of ranges) {
        if (i < r[0] || i > r[1]) out.push(r);
        else {
            if (r[0] < i) out.push([r[0], i - 1]);
            if (i < r[1]) out.push([i + 1, r[1]]);
        }
    }
    return out;
}

const countRanges = (ranges: Range[], total: number) =>
    ranges.reduce((n, [a, b]) => n + Math.max(0, Math.min(b, total - 1) - a + 1), 0);

// The row height is a CSS token (it changes with the density setting), so the
// virtualizer reads it instead of repeating the numbers.
function readRowHeight(): number {
    const v = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--row-h'));
    return v > 0 ? v : 28;
}

type Memory = {scroll: number; focus: number; ranges: Range[]; count: number; sort: Sort};
type Restore = {scroll: boolean; sel: boolean; mem: Memory};

// A reload of the folder on screen keeps the selection and the focused row on
// the same objects, wherever new or removed rows moved them: their keys are read
// before the reload and located in the new listing. Larger selections are not
// carried (Go's browse.MaxLocate).
const KEEP_MAX = 10000;
type Keep = {loc: string; keys: string[]; focusKey: string; focus: number; scroll: number};

// Builds ranges from row indexes in any order, skipping -1.
function rangesOf(indexes: number[]): Range[] {
    const sorted = indexes.filter((i) => i >= 0).sort((a, b) => a - b);
    const out: Range[] = [];
    for (const i of sorted) {
        const last = out[out.length - 1];
        if (last && i <= last[1] + 1) last[1] = Math.max(last[1], i);
        else out.push([i, i]);
    }
    return out;
}

export function Browser({profileId, location, onNavigate, canGoBack, onBack, onSnapshot, onSelection, density,
                            onUpload, onDownload, uploadBlocked, downloadBlocked, freshUploads,
                            onNewFolder, onDelete, refresh}: {
    profileId: string;
    location: Location | null;
    onNavigate: (prefix: string) => void;
    canGoBack: boolean;
    onBack: () => void;
    onSnapshot: (s: Snapshot | null) => void;
    onSelection: (s: SelectionInfo) => void;
    density: Density;
    /** Opens a picker and uploads into the folder shown. */
    onUpload: (what: 'files' | 'folder') => void;
    /** Downloads the selection. */
    onDownload: (sel: SelectionRequest) => void;
    /** Why uploading or downloading cannot start now, or ''. */
    uploadBlocked: string;
    downloadBlocked: string;
    /** Uploads into this folder finished since it was listed, when the folder
     *  is too large for the automatic reload; 0 otherwise. */
    freshUploads: number;
    /** Opens the new folder dialog for the folder shown. */
    onNewFolder: () => void;
    /** Asks to delete the selection. */
    onDelete: (sel: SelectionRequest) => void;
    /** Changing it reloads the listing, keeping the selection: after a delete, a
     *  new folder or finished uploads. */
    refresh: number;
}) {
    const [snap, setSnap] = useState<Snapshot | null>(null);
    const [sort, setSort] = useState<Sort>({field: 'name', desc: false});
    const [error, setError] = useState('');
    const [reload, setReload] = useState(0);
    const [focus, setFocus] = useState(0);
    const [sel, setSel] = useState<{ranges: Range[]; basis: string}>({ranges: noRanges, basis: ''});
    const [info, setInfo] = useState<SelectionInfo>(noSelection);
    const [rowH, setRowH] = useState(readRowHeight);
    const [, setTick] = useState(0);
    const [restored, setRestored] = useState(0);
    const anchor = useRef(-1);
    const listingId = useRef('');
    const mainRef = useRef<HTMLElement>(null);
    const scrollRef = useRef<HTMLDivElement>(null);
    // Set when the focused row should take the keyboard focus as soon as it is on
    // the page: after navigating, and after a key moved the focus. A wheel or
    // pointer action on the table cancels it.
    const pendingFocus = useRef(false);
    const restore = useRef<Restore | null>(null);
    const keep = useRef<Keep | null>(null);
    const memory = useRef(new Map<string, Memory>());
    const blocks = useRef(new Map<number, {stamp: string; rows: Row[]}>());
    const inflight = useRef(new Set<string>());
    const cacheKey = useRef('');
    const stampRef = useRef('');

    const bucket = location?.bucket ?? '';
    const prefix = location?.prefix ?? '';
    // The snapshot of the folder being shown; briefly stale right after navigating.
    const current = !!snap && snap.bucket === bucket && snap.prefix === prefix;
    const total = snap?.count ?? 0;
    const version = snap?.version ?? -1;
    const sortKey = `${snap?.id ?? ''}|${sort.field}|${sort.desc}`;
    const stamp = `${sortKey}|${version}`;
    stampRef.current = stamp;
    // Row blocks belong to one listing and one sort order. A new version of the
    // same order (more rows arrived) keeps the old blocks until they are refetched.
    if (cacheKey.current !== sortKey) {
        cacheKey.current = sortKey;
        blocks.current.clear();
        inflight.current.clear();
    }
    const ranges = sel.basis === stamp ? sel.ranges : noRanges;
    const live = useRef({focus, ranges, sort, total});
    live.current = {focus, ranges, sort, total};

    // Follow the density setting: --row-h is the single source of the row height.
    useLayoutEffect(() => {
        setRowH(readRowHeight());
        const mo = new MutationObserver(() => setRowH(readRowHeight()));
        mo.observe(document.documentElement, {attributes: true, attributeFilter: ['data-density']});
        return () => mo.disconnect();
    }, [density]);

    const virtual = useVirtualizer({
        count: total,
        getScrollElement: () => scrollRef.current,
        estimateSize: () => rowH,
        overscan: OVERSCAN,
        scrollMargin: HEAD_H,
        scrollPaddingStart: HEAD_H,
        // The focused row stays on the page when it scrolls out of view, so the
        // keyboard focus is not lost to the body.
        rangeExtractor: (range) => {
            const base = defaultRangeExtractor(range);
            const f = live.current.focus;
            if (f < 0 || f >= range.count || (f >= range.startIndex - OVERSCAN && f <= range.endIndex + OVERSCAN)) return base;
            return f < range.startIndex ? [f, ...base] : [...base, f];
        },
    });
    useEffect(() => virtual.measure(), [rowH, virtual]);
    const items = virtual.getVirtualItems();
    // The rows to fetch: those in view plus the overscan, not the kept focused row.
    const firstIdx = Math.max(0, (virtual.range?.startIndex ?? 0) - OVERSCAN);
    const lastIdx = (virtual.range?.endIndex ?? 0) + OVERSCAN;

    const getRow = (i: number): Row | undefined => blocks.current.get(Math.floor(i / BLOCK))?.rows[i % BLOCK];

    // Open a listing whenever the location changes, and close the old one. The
    // scroll position, focus and selection of the folder being left are kept so
    // that going back to it is cheap.
    useEffect(() => {
        setSnap(null);
        onSnapshot(null);
        setFocus(0);
        setError('');
        setSel({ranges: noRanges, basis: ''});
        anchor.current = -1;
        const active = document.activeElement;
        pendingFocus.current = !active || active === document.body || !!mainRef.current?.contains(active);
        const key = `${profileId}|${bucket}|${prefix}`;
        if (keep.current && keep.current.loc !== key) keep.current = null;
        const mem = memory.current.get(key);
        restore.current = mem && !keep.current ? {scroll: mem.scroll > 0, sel: mem.ranges.length > 0, mem} : null;
        if (scrollRef.current) scrollRef.current.scrollTop = 0;
        if (!bucket) return;
        let cancelled = false;
        OpenListing(profileId, bucket, prefix)
            .then((s) => {
                if (cancelled) {
                    CloseListing(s.id);
                    return;
                }
                listingId.current = s.id;
                setSnap(s as Snapshot);
                onSnapshot(s as Snapshot);
            })
            .catch((err) => !cancelled && setError(String(err)));
        return () => {
            cancelled = true;
            const l = live.current;
            if (l.total > 0) {
                const m = memory.current;
                m.delete(key);
                m.set(key, {scroll: scrollRef.current?.scrollTop ?? 0, focus: l.focus, ranges: l.ranges, count: l.total, sort: l.sort});
                if (m.size > 100) m.delete(m.keys().next().value as string);
            }
            if (listingId.current) CloseListing(listingId.current);
            listingId.current = '';
        };
        // onSnapshot is a state setter from the parent
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [profileId, bucket, prefix, reload]);

    useEffect(() => EventsOn('s4:listing', (s: Snapshot) => {
        if (s.id !== listingId.current) return;
        setSnap(s);
        onSnapshot(s);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }), []);

    // Fetch the blocks around the visible rows that are missing or out of date.
    useEffect(() => {
        const id = snap?.id;
        if (!id || total === 0) return;
        const last = Math.min(lastIdx, total - 1);
        for (let b = Math.floor(firstIdx / BLOCK); b <= Math.floor(last / BLOCK); b++) {
            const have = blocks.current.get(b);
            const tag = `${stamp}|${b}`;
            if ((have && have.stamp === stamp) || inflight.current.has(tag)) continue;
            inflight.current.add(tag);
            FetchWindow(id, sort.field, sort.desc, b * BLOCK, BLOCK)
                .then((w) => {
                    inflight.current.delete(tag);
                    if (stampRef.current !== stamp) return;
                    blocks.current.set(b, {stamp, rows: (w as ListingWindow).rows ?? []});
                    setTick((n) => n + 1);
                })
                .catch((err) => {
                    inflight.current.delete(tag);
                    setError(String(err));
                });
        }
    }, [stamp, firstIdx, lastIdx, total, snap?.id, sort.field, sort.desc]);

    // Report the selection to the details panel and status bar. Go totals the
    // ranges; a single row already on the page needs no request.
    const reportToken = useRef(0);
    useEffect(() => {
        const token = ++reportToken.current;
        const id = snap?.id;
        const n = countRanges(ranges, total);
        const done = (v: SelectionInfo) => {
            if (token !== reportToken.current) return;
            setInfo(v);
            onSelection(v);
        };
        if (!id || n === 0) {
            done(noSelection);
            return;
        }
        const idx = ranges[0][0];
        const cached = n === 1 ? getRow(idx) : undefined;
        if (cached) {
            done({count: 1, folders: cached.folder ? 1 : 0, objects: cached.folder ? 0 : 1, bytes: cached.folder ? 0 : cached.size, single: cached});
            return;
        }
        Promise.all([
            ListingSummary(id, sort.field, sort.desc, ranges),
            n === 1 ? FetchWindow(id, sort.field, sort.desc, idx, 1) : Promise.resolve(null),
        ]).then(([s, w]) => done({
            count: s.count, folders: s.folders, objects: s.objects, bytes: s.bytes,
            single: n === 1 ? ((w as ListingWindow | null)?.rows?.[0] ?? null) : null,
        })).catch((err) => setError(String(err)));
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [ranges, snap?.id, sort.field, sort.desc, total]);

    // Come back to a folder as it was left: scroll position, focus, and the
    // selection when the listing has the same rows in the same order.
    useEffect(() => {
        const r = restore.current;
        const el = scrollRef.current;
        if (!r || !snap || !current || snap.id !== listingId.current || !el) return;
        if (r.scroll && (HEAD_H + total * rowH >= r.mem.scroll + el.clientHeight || snap.state !== 'loading')) {
            el.scrollTop = r.mem.scroll;
            r.scroll = false;
        }
        if (r.sel && snap.state === 'done') {
            if (snap.count === r.mem.count && r.mem.sort.field === sort.field && r.mem.sort.desc === sort.desc) {
                setSel({ranges: r.mem.ranges, basis: stamp});
                setFocus(Math.min(r.mem.focus, Math.max(0, total - 1)));
                setRestored((n) => n + 1);
            }
            r.sel = false;
        }
        if (!r.scroll && !r.sel) restore.current = null;
    }, [snap?.state, total, rowH, stamp, sort.field, sort.desc]);

    // After a reload that keeps the view: the scroll position as soon as the rows
    // reach it, then the selection and focus once the listing is complete.
    useEffect(() => {
        const k = keep.current;
        const el = scrollRef.current;
        if (!k || !snap || !current || snap.id !== listingId.current || !el) return;
        if (k.scroll >= 0 && (HEAD_H + total * rowH >= k.scroll + el.clientHeight || snap.state !== 'loading')) {
            el.scrollTop = k.scroll;
            k.scroll = -1;
        }
        if (snap.state === 'loading') return;
        keep.current = null;
        if (snap.state !== 'done') return;
        const wanted = k.focusKey ? [...k.keys, k.focusKey] : k.keys;
        if (wanted.length === 0) {
            setFocus(Math.min(k.focus, Math.max(0, total - 1)));
            return;
        }
        const basis = stamp;
        ListingLocate(snap.id, sort.field, sort.desc, wanted)
            .then((idx) => {
                if (stampRef.current !== basis) return;
                setSel({ranges: rangesOf(idx.slice(0, k.keys.length)), basis});
                const f = k.focusKey ? idx[idx.length - 1] : -1;
                setFocus(f >= 0 ? f : Math.min(k.focus, Math.max(0, total - 1)));
                setRestored((n) => n + 1);
            })
            .catch(() => setFocus(Math.min(k.focus, Math.max(0, total - 1))));
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [snap?.state, snap?.id, total, rowH, stamp]);

    // Reloads the folder on screen and keeps the view: scroll, focus and the
    // selection, located by key in the new listing.
    const reloadKeeping = async () => {
        const loc = `${profileId}|${bucket}|${prefix}`;
        const id = current ? snap?.id : undefined;
        const n = countRanges(ranges, total);
        let keys: string[] = [];
        if (id && n > 0 && n <= KEEP_MAX) {
            try {
                keys = (await ListingKeys(id, sort.field, sort.desc, version, ranges)) ?? [];
            } catch {
                keys = []; // The listing changed or closed: reload without the selection.
            }
        }
        keep.current = {loc, keys, focusKey: (current && getRow(focus)?.key) || '', focus, scroll: scrollRef.current?.scrollTop ?? 0};
        setReload((r) => r + 1);
    };
    const reloadRef = useRef(reloadKeeping);
    reloadRef.current = reloadKeeping;

    // The parent asks for a reload by changing refresh.
    const firstRefresh = useRef(refresh);
    useEffect(() => {
        if (refresh === firstRefresh.current) return;
        void reloadRef.current();
    }, [refresh]);

    // F5 and Ctrl+R reload this folder's listing, not the page.
    useEffect(() => {
        const onKey = (e: globalThis.KeyboardEvent) => {
            if (e.key === 'F5' || ((e.ctrlKey || e.metaKey) && !e.shiftKey && (e.key === 'r' || e.key === 'R'))) {
                e.preventDefault();
                if (location) void reloadRef.current();
            }
        };
        window.addEventListener('keydown', onKey, true);
        return () => window.removeEventListener('keydown', onKey, true);
    }, [location]);

    const moveFocus = (j: number) => {
        setFocus(j);
        pendingFocus.current = true;
        virtual.scrollToIndex(j, {align: 'auto'});
    };

    // A restored focus moves the keyboard to its row, unless the user is elsewhere.
    useEffect(() => {
        const a = document.activeElement;
        if (restored > 0 && (!a || a === document.body || mainRef.current?.contains(a))) pendingFocus.current = true;
    }, [restored]);

    // Give the focus to the focused row once it is on the page. After navigating
    // this puts the keyboard in the new listing.
    useEffect(() => {
        if (!pendingFocus.current || (snap && (!current || snap.id !== listingId.current))) return;
        const el = scrollRef.current;
        if (!el) return;
        if (total === 0) {
            if (snap?.state === 'done') {
                el.focus({preventScroll: true});
                pendingFocus.current = false;
            }
            return;
        }
        const row = el.querySelector<HTMLElement>(`[data-index="${focus}"]`);
        if (row) {
            row.focus({preventScroll: true});
            pendingFocus.current = false;
        }
    });

    const select = (next: Range[]) => setSel({ranges: next, basis: stamp});

    const choose = (i: number, e: {ctrlKey: boolean; shiftKey: boolean; metaKey: boolean}) => {
        if (i < 0 || i >= total) return;
        setFocus(i);
        const ctrl = e.ctrlKey || e.metaKey;
        if (e.shiftKey) {
            const from = anchor.current >= 0 ? anchor.current : focus;
            select(addRange(ctrl ? ranges : noRanges, from, i));
            return;
        }
        anchor.current = i;
        if (ctrl) select(inRanges(ranges, i) ? removeIndex(ranges, i) : addRange(ranges, i, i));
        else select([[i, i]]);
    };

    const open = (row: Row | undefined) => {
        if (row?.folder) onNavigate(row.key);
    };

    const pageSize = () => Math.max(1, Math.floor((scrollRef.current?.clientHeight ?? 0) / rowH) - 2);

    const onTableKey = (e: KeyboardEvent) => {
        if ((e.target as HTMLElement).closest('.thead')) return;
        if (e.altKey) return;
        const move = (to: number) => {
            e.preventDefault();
            if (total === 0) return;
            const j = Math.max(0, Math.min(total - 1, to));
            choose(j, {ctrlKey: false, metaKey: false, shiftKey: e.shiftKey});
            moveFocus(j);
        };
        const mod = e.ctrlKey || e.metaKey;
        if (e.key === 'ArrowDown') move(focus + 1);
        else if (e.key === 'ArrowUp') move(focus - 1);
        else if (e.key === 'PageDown') move(focus + pageSize());
        else if (e.key === 'PageUp') move(focus - pageSize());
        else if (e.key === 'Home') move(0);
        else if (e.key === 'End') move(total - 1);
        else if (e.key === 'Enter') open(getRow(focus));
        else if (e.key === ' ' && mod) {
            e.preventDefault();
            choose(focus, {ctrlKey: true, metaKey: false, shiftKey: false});
        } else if (e.key === 'a' && mod) {
            e.preventDefault();
            if (total > 0) {
                anchor.current = 0;
                select([[0, total - 1]]);
            }
        } else if (e.key === 'Escape') select(noRanges);
        else if (e.key === 'Delete' && !mod && !e.shiftKey && selectionRequest()) {
            e.preventDefault();
            onDelete(selectionRequest()!);
        }
    };

    const goUp = () => {
        const parts = prefix.split('/').filter((_, i, all) => i < all.length - 1);
        onNavigate(parts.length <= 1 ? '' : parts.slice(0, -1).join('/') + '/');
    };

    // Up one level, or Backspace anywhere in the listing.
    const onAreaKey = (e: KeyboardEvent) => {
        if ((e.key === 'Backspace' || (e.key === 'ArrowUp' && e.altKey)) && prefix) {
            e.preventDefault();
            goUp();
        }
    };

    const sortBy = (field: SortField) => {
        setSort((s) => ({field, desc: s.field === field ? !s.desc : false}));
        setFocus(0);
        anchor.current = -1;
        if (scrollRef.current) scrollRef.current.scrollTop = 0;
    };

    const header = (field: SortField, label: string, right = false) => {
        const on = sort.field === field;
        return (
            <div role="columnheader" aria-sort={on ? (sort.desc ? 'descending' : 'ascending') : 'none'}>
                <button type="button" className={`th${right ? ' right' : ''}${on ? ' sorted' : ''}`} onClick={() => sortBy(field)}
                        style={right ? {width: '100%'} : undefined}>
                    {label}{on && <Icon name={sort.desc ? 'chevronDown' : 'chevronUp'} size="xs"/>}
                </button>
            </div>
        );
    };

    // Path segments of the prefix, each with the prefix that opens it.
    const segments = prefix.split('/').slice(0, -1).map((name, i, all) => ({
        name: name === '' ? '/' : name,
        prefix: all.slice(0, i + 1).join('/') + '/',
    }));

    const selCount = info.count;
    // The selection as Go resolves it: index ranges of this listing version.
    function selectionRequest(): SelectionRequest | null {
        if (!snap || !current || info.count === 0 || ranges.length === 0) return null;
        return {
            listingId: snap.id, field: sort.field, desc: sort.desc, version: snap.version, ranges,
            count: info.count, folders: info.folders, objects: info.objects, bytes: info.bytes,
        };
    }
    const dropStyle = (bucket ? {'--wails-drop-target': 'drop'} : {}) as CSSProperties;
    const focusRendered = total > 0 && items.some((v) => v.index === focus);

    return (
        <main className="browser" aria-label="Objects" ref={mainRef}>
            <div className="pathbar">
                <button type="button" className="btn ghost icon-only" aria-label="Back" title="Back" disabled={!canGoBack} onClick={onBack}>
                    <Icon name="back"/>
                </button>
                <button type="button" className="btn ghost icon-only" aria-label="Up one folder" title="Up one folder (Backspace)"
                        disabled={!prefix} onClick={goUp}>
                    <Icon name="up"/>
                </button>
                <nav className="crumbs" aria-label="Path">
                    {bucket ? (
                        <>
                            <button type="button" className={`crumb root${prefix ? '' : ' current'}`} onClick={() => onNavigate('')}
                                    aria-current={prefix ? undefined : 'location'}>{bucket}</button>
                            <span className="crumb-sep">/</span>
                            {segments.map((s, i) => (
                                <span key={s.prefix} style={{display: 'contents'}}>
                                    <button type="button" className={`crumb${i === segments.length - 1 ? ' current' : ''}`}
                                            aria-current={i === segments.length - 1 ? 'location' : undefined}
                                            onClick={() => onNavigate(s.prefix)}>{s.name}</button>
                                    <span className="crumb-sep">/</span>
                                </span>
                            ))}
                        </>
                    ) : <span className="meta" style={{padding: '0 4px'}}>No bucket open</span>}
                </nav>
                <button type="button" className="btn ghost icon-only" aria-label="Reload listing" title="Reload listing (F5)"
                        disabled={!bucket} onClick={() => void reloadKeeping()}>
                    <Icon name="refresh"/>
                </button>
                <div className="vsep" style={{margin: '0 4px'}}/>
                <button type="button" className="btn" disabled={!bucket || !snap} aria-label="New folder"
                        title={bucket ? `Create a folder in ${bucket}/${prefix}` : 'Open a bucket first'} onClick={onNewFolder}>
                    <Icon name="folderPlus"/><span className="btn-label">New folder</span>
                </button>
                <button type="button" className="btn" disabled={!selCount || !!downloadBlocked || !snap}
                        aria-label={selCount ? `Download ${plural(selCount, 'selected item')}` : 'Download'}
                        title={downloadBlocked || (selCount ? `Download ${plural(selCount, 'selected item')} to a folder on this computer` : 'Select objects or folders to download')}
                        onClick={() => { const r = selectionRequest(); if (r) onDownload(r); }}>
                    <Icon name="download"/><span className="btn-label">Download</span>{selCount ? ` ${selCount.toLocaleString('en-US')}` : ''}
                </button>
                <button type="button" className="btn ghost icon-only danger-text" disabled={!selCount || !snap}
                        aria-label={selCount ? `Delete ${plural(selCount, 'selected item')}` : 'Delete'}
                        title={selCount ? `Delete ${plural(selCount, 'selected item')} (Delete)` : 'Select objects or folders to delete'}
                        onClick={() => { const r = selectionRequest(); if (r) onDelete(r); }}>
                    <Icon name="trash"/>
                </button>
                <UploadMenu disabled={!bucket || !!uploadBlocked} title={uploadBlocked || (bucket ? `Upload to ${bucket}/${prefix}` : 'Open a bucket first')}
                            onUpload={onUpload}/>
            </div>

            <div className="listing-area" style={dropStyle} onKeyDown={onAreaKey}>
                {(error || snap?.error) && (
                    <div className="banner danger" role="alert">
                        <Icon name="alert"/>
                        <div style={{flex: 1}}>
                            <strong>The listing could not be loaded</strong>
                            {error || snap?.error}
                        </div>
                        <button type="button" className="btn small" onClick={() => setReload((n) => n + 1)}>Try again</button>
                    </div>
                )}
                {!bucket ? (
                    <div className="empty">
                        <Icon name="bucket"/>
                        <strong>Open a bucket</strong>
                        <span>Choose one on the left, or add one by name.</span>
                    </div>
                ) : (
                    <div className="table-scroll" ref={scrollRef} tabIndex={focusRendered ? -1 : 0} onKeyDown={onTableKey}
                         onWheel={() => { pendingFocus.current = false; }} onPointerDown={() => { pendingFocus.current = false; }}>
                        <div className="table" role="table" aria-label={`${bucket}/${prefix}`} aria-rowcount={total + 1} aria-multiselectable="true">
                            <div className="cols thead" role="row" aria-rowindex={1}>
                                {header('name', 'Name')}
                                {header('size', 'Size', true)}
                                {header('modified', 'Modified')}
                                {header('class', 'Storage class')}
                            </div>
                            <div className="vbody" role="rowgroup" style={{height: Math.max(0, virtual.getTotalSize() - HEAD_H)}}>
                                {items.map((v) => {
                                    const i = v.index;
                                    const r = getRow(i);
                                    const on = inRanges(ranges, i);
                                    const style = {transform: `translateY(${v.start - HEAD_H}px)`};
                                    if (!r) {
                                        return (
                                            <div key={i} role="row" data-index={i} aria-rowindex={i + 2} aria-selected={on} aria-busy="true"
                                                 tabIndex={i === focus ? 0 : -1} style={style}
                                                 className={`cols trow vrow${on ? ' sel' : ''}`}
                                                 onClick={(e: MouseEvent) => choose(i, e)}>
                                                <div role="cell" className="name"><span className="skel"/></div>
                                                <div role="cell"/><div role="cell"/><div role="cell"/>
                                            </div>
                                        );
                                    }
                                    return (
                                        <div key={i} role="row" data-index={i} aria-rowindex={i + 2} aria-selected={on}
                                             tabIndex={i === focus ? 0 : -1} style={style}
                                             className={`cols trow vrow${on ? ' sel' : ''}`}
                                             onClick={(e: MouseEvent) => choose(i, e)} onDoubleClick={() => open(r)}>
                                            <div role="cell" className="name">
                                                <Icon name={r.folder ? 'folder' : 'file'} className={r.folder ? 'folder-icon' : 'file-icon'}/>
                                                <span className="ellipsis">{r.name}</span>
                                            </div>
                                            <div role="cell" className="num ellipsis">{r.folder ? '' : formatSize(r.size)}</div>
                                            <div role="cell" className="soft ellipsis">{r.folder ? '' : formatDate(r.modified)}</div>
                                            <div role="cell" className="soft ellipsis">{formatStorageClass(r.storageClass)}</div>
                                        </div>
                                    );
                                })}
                            </div>
                        </div>
                        {snap?.state === 'done' && total === 0 && !snap.error && (
                            <div className="empty" style={{height: 200}}>
                                <span>This folder is empty.</span>
                            </div>
                        )}
                        {!snap && !error && <div className="empty" style={{height: 200}}><span>Opening…</span></div>}
                    </div>
                )}
                <div className="drop-overlay" aria-hidden="true">
                    <Icon name="upload"/>
                    <div className="title">Drop to upload into</div>
                    <div className="path mono">{bucket}/{prefix}</div>
                    <div className="meta" style={{color: 'var(--text2)'}}>Folders upload with everything inside them.</div>
                </div>
            </div>

            <div className="statusbar" role="status">
                {snap && (
                    <>
                        <span className="strong">{plural(snap.objects, 'object')}, {plural(snap.folders, 'folder')}</span>
                        {snap.state === 'loading'
                            ? <span className="wide-only">Loading… {plural(snap.pages, 'page')}</span>
                            : snap.state === 'done' ? <span className="wide-only">Loaded in {(snap.elapsedMs / 1000).toFixed(1)} s</span> : null}
                        {snap.truncated && <span className="t-warn">Stopped at the 1,000,000 row limit</span>}
                        {snap.region && <span className="mono wide-only" style={{fontSize: 'var(--fs-label)'}}>{snap.region}</span>}
                    </>
                )}
                <div className="spacer"/>
                {freshUploads > 0 && (
                    <button type="button" className="btn ghost small" onClick={() => void reloadKeeping()}
                            title="This folder is too large to reload on its own. Reload it to see the new objects.">
                        <Icon name="refresh" size="sm"/>{plural(freshUploads, 'upload')} finished here · Reload
                    </button>
                )}
                {selCount > 0 && (
                    <span className="strong">
                        {plural(selCount, 'selected item')}{info.objects ? ` · ${formatSize(info.bytes)}` : ''}
                    </span>
                )}
            </div>
        </main>
    );
}

// The Upload button: files or a folder, each through the native picker.
function UploadMenu({disabled, title, onUpload}: {disabled: boolean; title: string; onUpload: (what: 'files' | 'folder') => void}) {
    const [open, setOpen] = useState(false);
    const btn = useRef<HTMLButtonElement>(null);
    const menu = useRef<HTMLDivElement>(null);
    useDismiss(open, () => setOpen(false), [btn, menu]);
    const pick = (what: 'files' | 'folder') => {
        setOpen(false);
        onUpload(what);
    };
    return (
        <div className="anchor">
            <button ref={btn} type="button" className="btn primary" disabled={disabled} title={title}
                    aria-haspopup="menu" aria-expanded={open} aria-label="Upload" onClick={() => setOpen((o) => !o)}>
                <Icon name="upload"/><span className="btn-label">Upload</span><Icon name="chevronDown" size="xs"/>
            </button>
            {open && (
                <div ref={menu} className="menu" role="menu" aria-label="Upload" style={{top: 32, right: 0, minWidth: 200}}>
                    <button type="button" role="menuitem" className="menu-item" onClick={() => pick('files')}>
                        <Icon name="file"/> Files…
                    </button>
                    <button type="button" role="menuitem" className="menu-item" onClick={() => pick('folder')}>
                        <Icon name="folder"/> Folder…
                    </button>
                    <p className="meta" style={{padding: '4px 8px'}}>Or drop files and folders on the listing.</p>
                </div>
            )}
        </div>
    );
}
