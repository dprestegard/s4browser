import {FormEvent, useCallback, useEffect, useRef, useState} from 'react';
import {
    AddBucket,
    Buckets,
    CloseListing,
    ListingWindow,
    OpenListing,
    RemoveBucket,
} from '../wailsjs/go/main/App';
import {EventsOn} from '../wailsjs/runtime/runtime';

// Stage 1.2 working panel: bucket list, add by name, and a paged view of a
// listing that Go loads, sorts and slices. Stage 1.3 replaces the look and
// stage 1.4 replaces the pager with a virtualized table. Only the visible page
// of rows ever reaches this page.

type Bucket = {name: string; region?: string; listed: boolean; added: boolean};
type BucketList = {buckets: Bucket[]; listError?: string; listDenied?: boolean};
type Snapshot = {
    id: string;
    bucket: string;
    prefix: string;
    region?: string;
    state: 'loading' | 'done' | 'error' | 'cancelled';
    count: number;
    folders: number;
    objects: number;
    pages: number;
    truncated?: boolean;
    error?: string;
    version: number;
    elapsedMs: number;
};
type Row = {key: string; name: string; folder: boolean; size: number; modified?: string; storageClass?: string};
type Window = {listing: Snapshot; offset: number; rows: Row[]};
type Field = 'name' | 'size' | 'modified' | 'class';

const pageSize = 100;

function formatSize(n: number): string {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let i = 0;
    let v = n;
    while (v >= 1024 && i < units.length - 1) {
        v /= 1024;
        i++;
    }
    return i === 0 ? `${n} B` : `${v.toFixed(1)} ${units[i]}`;
}

export function Browse({profile}: {profile: string}) {
    const [list, setList] = useState<BucketList>({buckets: []});
    const [message, setMessage] = useState('');
    const [form, setForm] = useState({name: '', region: ''});
    const [bucket, setBucket] = useState('');
    const [prefix, setPrefix] = useState('');
    const [snap, setSnap] = useState<Snapshot | null>(null);
    const [rows, setRows] = useState<Row[]>([]);
    const [sort, setSort] = useState<{field: Field; desc: boolean}>({field: 'name', desc: false});
    const [offset, setOffset] = useState(0);
    const [fetchMs, setFetchMs] = useState(0);
    const listingId = useRef('');

    const loadBuckets = useCallback(async () => {
        setMessage('');
        try {
            setList((await Buckets(profile)) as BucketList);
        } catch (err) {
            setList({buckets: []});
            setMessage(String(err));
        }
    }, [profile]);

    useEffect(() => {
        loadBuckets();
    }, [loadBuckets]);

    // Open a listing whenever the location changes, and close the old one.
    useEffect(() => {
        if (!bucket) return;
        let cancelled = false;
        setRows([]);
        setSnap(null);
        setOffset(0);
        OpenListing(profile, bucket, prefix)
            .then((s) => {
                if (cancelled) {
                    CloseListing(s.id);
                    return;
                }
                listingId.current = s.id;
                setSnap(s as Snapshot);
            })
            .catch((err) => setMessage(String(err)));
        return () => {
            cancelled = true;
            if (listingId.current) CloseListing(listingId.current);
            listingId.current = '';
        };
    }, [profile, bucket, prefix]);

    useEffect(
        () =>
            EventsOn('s4:listing', (s: Snapshot) => {
                if (s.id === listingId.current) setSnap(s);
            }),
        [],
    );

    // Fetch the visible page whenever the listing grows, or sort or page change.
    const version = snap?.version ?? -1;
    useEffect(() => {
        const id = listingId.current;
        if (!id) return;
        const t0 = performance.now();
        ListingWindow(id, sort.field, sort.desc, offset, pageSize)
            .then((w) => {
                if (id !== listingId.current) return;
                setFetchMs(performance.now() - t0);
                setRows((w as Window).rows);
            })
            .catch((err) => setMessage(String(err)));
    }, [version, sort, offset]);

    const add = async (e: FormEvent) => {
        e.preventDefault();
        setMessage('');
        try {
            const r = await AddBucket(profile, form.name, form.region);
            setForm({name: '', region: ''});
            setMessage(`Added ${r.bucket.name} (${r.bucket.region}).${r.note ? ' ' + r.note : ''}`);
        } catch (err) {
            setMessage(String(err));
        }
        await loadBuckets();
    };

    const remove = async (name: string) => {
        try {
            await RemoveBucket(profile, name);
        } catch (err) {
            setMessage(String(err));
        }
        await loadBuckets();
    };

    const header = (field: Field, label: string) => (
        <th>
            <button
                type="button"
                className="sort"
                onClick={() => {
                    setOffset(0);
                    setSort((s) => ({field, desc: s.field === field ? !s.desc : false}));
                }}
            >
                {label}
                {sort.field === field ? (sort.desc ? ' ▼' : ' ▲') : ''}
            </button>
        </th>
    );

    const segments = prefix.split('/').filter((p, i, all) => i < all.length - 1 || p !== '');
    const total = snap?.count ?? 0;

    return (
        <section className="browse">
            <h2>Buckets</h2>
            {!profile && <p className="meta">No profile is selected.</p>}
            {list.listError && <p className="problem">{list.listError}{list.listDenied ? ' Add buckets by name below.' : ''}</p>}
            <ul>
                {list.buckets.map((b) => (
                    <li key={b.name}>
                        <button type="button" className={b.name === bucket ? 'current' : ''} onClick={() => {
                            setBucket(b.name);
                            setPrefix('');
                        }}>{b.name}</button>
                        <span className="meta"> {b.region ?? 'region unknown'}{b.added ? ', added by name' : ''}</span>
                        {b.added && <button type="button" onClick={() => remove(b.name)}>Forget</button>}
                    </li>
                ))}
            </ul>
            <button type="button" onClick={loadBuckets}>Refresh buckets</button>
            <form onSubmit={add} autoComplete="off">
                <input aria-label="Bucket name" placeholder="Bucket name" required spellCheck={false}
                       value={form.name} onChange={(e) => setForm({...form, name: e.target.value})}/>
                <input aria-label="Region" placeholder="Region (only if detection fails)" spellCheck={false}
                       value={form.region} onChange={(e) => setForm({...form, region: e.target.value})}/>
                <button type="submit">Add bucket</button>
            </form>
            {message && <p role="status" className="status">{message}</p>}

            {bucket && (
                <>
                    <h2>
                        <button type="button" className="crumb" onClick={() => setPrefix('')}>{bucket}</button>
                        {segments.map((seg, i) => (
                            <span key={i}>
                                {' / '}
                                <button type="button" className="crumb"
                                        onClick={() => setPrefix(segments.slice(0, i + 1).join('/') + '/')}>{seg}</button>
                            </span>
                        ))}
                    </h2>
                    {snap && (
                        <p className="meta" role="status">
                            {snap.state === 'loading' ? 'Loading' : snap.state}: {snap.folders.toLocaleString()} folders,{' '}
                            {snap.objects.toLocaleString()} objects, {snap.pages} pages, {(snap.elapsedMs / 1000).toFixed(2)} s
                            {snap.region ? `, ${snap.region}` : ''}
                            {snap.truncated ? ', stopped at the row limit' : ''}
                            {`, last page of rows fetched in ${fetchMs.toFixed(1)} ms`}
                        </p>
                    )}
                    {snap?.error && <p className="problem">{snap.error}</p>}
                    <div className="pager">
                        <button type="button" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - pageSize))}>Previous</button>
                        <span className="meta">
                            {' '}rows {total === 0 ? 0 : offset + 1}–{Math.min(offset + pageSize, total)} of {total.toLocaleString()}{' '}
                        </span>
                        <button type="button" disabled={offset + pageSize >= total} onClick={() => setOffset(offset + pageSize)}>Next</button>
                    </div>
                    <table className="listing">
                        <thead>
                            <tr>
                                {header('name', 'Name')}
                                {header('size', 'Size')}
                                {header('modified', 'Modified')}
                                {header('class', 'Class')}
                            </tr>
                        </thead>
                        <tbody>
                            {rows.map((r) => (
                                <tr key={r.key}>
                                    <td>
                                        {r.folder ? (
                                            <button type="button" className="crumb" onClick={() => setPrefix(r.key)}>📁 {r.name}</button>
                                        ) : r.name}
                                    </td>
                                    <td>{r.folder ? '' : formatSize(r.size)}</td>
                                    <td>{r.modified ? new Date(r.modified).toLocaleString() : ''}</td>
                                    <td>{r.storageClass ?? ''}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </>
            )}
        </section>
    );
}
