import {CSSProperties, KeyboardEvent, MouseEvent, useEffect, useRef, useState} from 'react';
import {CloseListing, ListingWindow as FetchWindow, OpenListing} from '../../wailsjs/go/main/App';
import {EventsOn} from '../../wailsjs/runtime/runtime';
import {Icon} from '../ui/Icon';
import {formatDate, formatSize, formatStorageClass, plural} from '../ui/format';
import {ListingWindow, Location, Row, Snapshot, Sort, SortField} from '../types';

// Rows are fetched from Go one page at a time; Go owns the listing and its sort.
// Stage 1.4 replaces the pager with a virtualized table that asks only for the
// visible rows.
const pageSize = 200;

export function Browser({profileId, location, onNavigate, canGoBack, onBack, onSnapshot, selected, onSelect}: {
    profileId: string;
    location: Location | null;
    onNavigate: (prefix: string) => void;
    canGoBack: boolean;
    onBack: () => void;
    onSnapshot: (s: Snapshot | null) => void;
    selected: Row[];
    onSelect: (rows: Row[]) => void;
}) {
    const [snap, setSnap] = useState<Snapshot | null>(null);
    const [rows, setRows] = useState<Row[]>([]);
    const [sort, setSort] = useState<Sort>({field: 'name', desc: false});
    const [offset, setOffset] = useState(0);
    const [error, setError] = useState('');
    const [reload, setReload] = useState(0);
    const [focus, setFocus] = useState(0);
    const anchor = useRef(-1);
    const listingId = useRef('');
    const tableRef = useRef<HTMLDivElement>(null);

    const bucket = location?.bucket ?? '';
    const prefix = location?.prefix ?? '';

    // Open a listing whenever the location changes, and close the old one.
    useEffect(() => {
        setRows([]);
        setSnap(null);
        onSnapshot(null);
        setOffset(0);
        setFocus(0);
        setError('');
        anchor.current = -1;
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

    // Fetch the page whenever the listing grows, or the sort or page changes.
    const version = snap?.version ?? -1;
    useEffect(() => {
        const id = listingId.current;
        if (!id) return;
        FetchWindow(id, sort.field, sort.desc, offset, pageSize)
            .then((w) => {
                if (id === listingId.current) setRows((w as ListingWindow).rows ?? []);
            })
            .catch((err) => setError(String(err)));
    }, [version, sort, offset]);

    const selectedKeys = new Set(selected.map((r) => r.key));

    const choose = (i: number, e: {ctrlKey: boolean; shiftKey: boolean; metaKey: boolean}) => {
        const row = rows[i];
        if (!row) return;
        setFocus(i);
        if (e.shiftKey && anchor.current >= 0) {
            const [a, b] = anchor.current < i ? [anchor.current, i] : [i, anchor.current];
            const range = rows.slice(a, b + 1);
            const keep = e.ctrlKey || e.metaKey ? selected.filter((r) => !range.some((x) => x.key === r.key)) : [];
            onSelect([...keep, ...range]);
            return;
        }
        anchor.current = i;
        if (e.ctrlKey || e.metaKey) {
            onSelect(selectedKeys.has(row.key) ? selected.filter((r) => r.key !== row.key) : [...selected, row]);
        } else {
            onSelect([row]);
        }
    };

    const open = (row: Row) => {
        if (row.folder) onNavigate(row.key);
    };

    const focusRow = (i: number) => {
        const el = tableRef.current?.querySelector<HTMLElement>(`[data-index="${i}"]`);
        el?.focus();
        el?.scrollIntoView({block: 'nearest'});
    };

    const onRowKey = (i: number, e: KeyboardEvent) => {
        const move = (to: number) => {
            e.preventDefault();
            const j = Math.max(0, Math.min(rows.length - 1, to));
            choose(j, {ctrlKey: false, metaKey: false, shiftKey: e.shiftKey});
            focusRow(j);
        };
        if (e.key === 'ArrowDown') move(i + 1);
        else if (e.key === 'ArrowUp') move(i - 1);
        else if (e.key === 'Home') move(0);
        else if (e.key === 'End') move(rows.length - 1);
        else if (e.key === 'Enter') open(rows[i]);
        else if (e.key === ' ' && (e.ctrlKey || e.metaKey)) {
            e.preventDefault();
            choose(i, {ctrlKey: true, metaKey: false, shiftKey: false});
        } else if (e.key === 'a' && (e.ctrlKey || e.metaKey)) {
            e.preventDefault();
            onSelect(rows);
        } else if (e.key === 'Escape') onSelect([]);
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
        setOffset(0);
        setSort((s) => ({field, desc: s.field === field ? !s.desc : false}));
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

    const selCount = selected.length;
    const selObjects = selected.filter((r) => !r.folder);
    const selBytes = selObjects.reduce((n, r) => n + r.size, 0);
    const total = snap?.count ?? 0;
    const dropStyle = (bucket ? {'--wails-drop-target': 'drop'} : {}) as CSSProperties;

    return (
        <main className="browser" aria-label="Objects">
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
                <button type="button" className="btn ghost icon-only" aria-label="Reload listing" title="Reload listing"
                        disabled={!bucket} onClick={() => setReload((n) => n + 1)}>
                    <Icon name="refresh"/>
                </button>
                <div className="vsep" style={{margin: '0 4px'}}/>
                <button type="button" className="btn" disabled aria-label="New folder" title="Creating folders arrives in stage 3.1">
                    <Icon name="folderPlus"/><span className="btn-label">New folder</span>
                </button>
                <button type="button" className="btn" disabled aria-label={selCount ? `Download ${plural(selCount, "selected item")}` : "Download"} title="Downloading arrives in stage 2.4">
                    <Icon name="download"/><span className="btn-label">Download</span>{selCount ? ` ${selCount}` : ''}
                </button>
                <button type="button" className="btn ghost icon-only danger-text" disabled
                        aria-label={selCount ? `Delete ${plural(selCount, 'selected item')}` : 'Delete'} title="Deleting arrives in stage 3.1">
                    <Icon name="trash"/>
                </button>
                <button type="button" className="btn primary" disabled aria-label="Upload" title="Uploading arrives in stage 2.4">
                    <Icon name="upload"/><span className="btn-label">Upload</span>
                </button>
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
                    <div className="table-scroll" ref={tableRef}>
                        <div className="table" role="table" aria-label={`${bucket}/${prefix}`} aria-rowcount={total + 1}>
                            <div className="cols thead" role="row">
                                {header('name', 'Name')}
                                {header('size', 'Size', true)}
                                {header('modified', 'Modified')}
                                {header('class', 'Storage class')}
                            </div>
                            {rows.map((r, i) => (
                                <div key={r.key} role="row" data-index={i} aria-rowindex={offset + i + 2}
                                     aria-selected={selectedKeys.has(r.key)} tabIndex={i === focus ? 0 : -1}
                                     className={`cols trow${selectedKeys.has(r.key) ? ' sel' : ''}`}
                                     onClick={(e: MouseEvent) => choose(i, e)}
                                     onDoubleClick={() => open(r)}
                                     onKeyDown={(e) => onRowKey(i, e)}>
                                    <div role="cell" className="name">
                                        <Icon name={r.folder ? 'folder' : 'file'} className={r.folder ? 'folder-icon' : 'file-icon'}/>
                                        <span className="ellipsis">{r.name}</span>
                                    </div>
                                    <div role="cell" className="num ellipsis">{r.folder ? '' : formatSize(r.size)}</div>
                                    <div role="cell" className="soft ellipsis">{r.folder ? '' : formatDate(r.modified)}</div>
                                    <div role="cell" className="soft ellipsis">{formatStorageClass(r.storageClass)}</div>
                                </div>
                            ))}
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
                {selCount > 0 && (
                    <span className="strong">
                        {plural(selCount, 'selected item')}{selObjects.length ? ` · ${formatSize(selBytes)}` : ''}
                    </span>
                )}
                {total > pageSize && (
                    <span className="pager">
                        <button type="button" className="btn ghost small icon-only" aria-label="Previous rows" disabled={offset === 0}
                                onClick={() => setOffset(Math.max(0, offset - pageSize))}><Icon name="chevronLeft" size="sm"/></button>
                        <span>{(offset + 1).toLocaleString('en-US')}–{Math.min(offset + pageSize, total).toLocaleString('en-US')} of {total.toLocaleString('en-US')}</span>
                        <button type="button" className="btn ghost small icon-only" aria-label="Next rows" disabled={offset + pageSize >= total}
                                onClick={() => setOffset(offset + pageSize)}><Icon name="chevronRight" size="sm"/></button>
                    </span>
                )}
            </div>
        </main>
    );
}
