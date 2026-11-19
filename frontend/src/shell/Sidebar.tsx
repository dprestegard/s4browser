import {FormEvent, useCallback, useEffect, useState} from 'react';
import {AddBucket, Buckets, RemoveBucket} from '../../wailsjs/go/main/App';
import {Icon} from '../ui/Icon';
import {Dialog} from '../ui/overlay';
import {Bucket, BucketList} from '../types';

// The bucket list of the active profile, with buckets added by name merged in.
export function Sidebar({profileId, current, onOpen, width}: {
    profileId: string;
    current: string;
    onOpen: (bucket: Bucket) => void;
    width: number;
}) {
    const [list, setList] = useState<BucketList | null>(null);
    const [error, setError] = useState('');
    const [note, setNote] = useState('');
    const [adding, setAdding] = useState(false);

    const load = useCallback(async () => {
        setError('');
        try {
            setList((await Buckets(profileId)) as BucketList);
        } catch (err) {
            setList({buckets: []});
            setError(String(err));
        }
    }, [profileId]);

    useEffect(() => {
        setList(null);
        setNote('');
        load();
    }, [load]);

    const forget = async (name: string) => {
        try {
            await RemoveBucket(profileId, name);
            setNote('');
        } catch (err) {
            setError(String(err));
        }
        await load();
    };

    const buckets = list?.buckets ?? [];
    return (
        <nav className="sidebar" aria-label="Buckets" style={{width}}>
            <div className="sidebar-head">
                <span className="label">Buckets</span>
                <span style={{display: 'flex', gap: 2}}>
                    <button type="button" className="btn ghost small icon-only" aria-label="Reload bucket list"
                            title="Reload bucket list" onClick={load}><Icon name="refresh" size="sm"/></button>
                    <button type="button" className="btn ghost small icon-only" aria-label="Add a bucket by name"
                            title="Add a bucket by name" onClick={() => setAdding(true)}><Icon name="plus"/></button>
                </span>
            </div>
            {list === null && <p className="sidebar-note">Loading buckets…</p>}
            {buckets.map((b) => (
                <div key={b.name} className="bucket-item">
                    <button type="button" className={`bucket${b.name === current ? ' on' : ''}`}
                            aria-current={b.name === current ? 'true' : undefined} onClick={() => onOpen(b)}>
                        <Icon name="bucket"/>
                        <span className="ellipsis" style={{flex: 1}}>{b.name}</span>
                        <span className="region">{b.region ?? '?'}</span>
                    </button>
                    {b.added && (
                        <button type="button" className="btn ghost small icon-only forget" aria-label={`Forget ${b.name}`}
                                title={`Forget ${b.name} (it stays in S3)`} onClick={() => forget(b.name)}>
                            <Icon name="close" size="sm"/>
                        </button>
                    )}
                </div>
            ))}
            {list?.listDenied && (
                <div className="sidebar-note" style={{display: 'flex', flexDirection: 'column', gap: 10}}>
                    <span style={{color: 'var(--text2)'}}>This profile is not allowed to list buckets. Add the ones you use by name.</span>
                    {buckets.length === 0 && (
                        <button type="button" className="btn" style={{alignSelf: 'flex-start'}} onClick={() => setAdding(true)}>
                            <Icon name="plus"/>Add bucket
                        </button>
                    )}
                </div>
            )}
            {list && !list.listDenied && list.listError && <p className="sidebar-note problem">{list.listError}</p>}
            {error && <p className="sidebar-note problem" role="alert">{error}</p>}
            {list && !error && !list.listError && buckets.length === 0 && (
                <p className="sidebar-note">No buckets. Add one by name with +.</p>
            )}
            {buckets.some((b) => b.added) && !list?.listDenied && (
                <p className="sidebar-note">{buckets.filter((b) => b.added).map((b) => b.name).join(', ')} {buckets.filter((b) => b.added).length === 1 ? 'was' : 'were'} added by name.</p>
            )}
            {note && <p className="sidebar-note" role="status">{note}</p>}
            {adding && (
                <AddBucketDialog profileId={profileId} onClose={() => setAdding(false)} onAdded={async (b, n) => {
                    setAdding(false);
                    setNote(n ?? '');
                    await load();
                    onOpen(b);
                }}/>
            )}
        </nav>
    );
}

function AddBucketDialog({profileId, onClose, onAdded}: {
    profileId: string;
    onClose: () => void;
    onAdded: (b: Bucket, note?: string) => void;
}) {
    const [name, setName] = useState('');
    const [region, setRegion] = useState('');
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);

    const submit = async (e: FormEvent) => {
        e.preventDefault();
        setError('');
        setBusy(true);
        try {
            const r = await AddBucket(profileId, name.trim(), region.trim());
            onAdded(r.bucket as Bucket, r.note);
        } catch (err) {
            setError(String(err));
        } finally {
            setBusy(false);
        }
    };

    return (
        <Dialog title="Add a bucket by name" onClose={onClose}>
            <form onSubmit={submit} autoComplete="off" style={{display: 'flex', flexDirection: 'column', gap: 16}}>
                <div className="field">
                    <label htmlFor="b-name">Bucket name</label>
                    <input id="b-name" className="input" required data-autofocus spellCheck={false}
                           value={name} onChange={(e) => setName(e.target.value)}/>
                </div>
                <div className="field">
                    <label htmlFor="b-region">Region <span className="hint">(optional, used only if detection fails)</span></label>
                    <input id="b-region" className="input" placeholder="Detect automatically" spellCheck={false}
                           value={region} onChange={(e) => setRegion(e.target.value)}/>
                </div>
                {error && <p className="t-danger" role="alert">{error}</p>}
                <div className="actions">
                    <button type="button" className="btn" onClick={onClose}>Cancel</button>
                    <button type="submit" className="btn primary" disabled={busy}>{busy ? 'Finding bucket…' : 'Add bucket'}</button>
                </div>
            </form>
        </Dialog>
    );
}
