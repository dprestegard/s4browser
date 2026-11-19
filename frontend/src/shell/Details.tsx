import {formatBytes, formatDate, formatSize, formatStorageClass, plural} from '../ui/format';
import {Location, Row, SelectionInfo, Snapshot} from '../types';

// The right-hand panel. It shows what the listing already knows. Encryption,
// checksums, tags and presigned links need extra requests and arrive in stage 3.1.
export function Details({location, snapshot, selection}: {
    location: Location | null;
    snapshot: Snapshot | null;
    selection: SelectionInfo;
}) {
    return (
        <aside className="details selectable" aria-label="Details">
            {selection.count === 1 && selection.single ? <One row={selection.single} location={location}/>
                : selection.count > 0 ? <Many info={selection}/>
                    : <Folder location={location} snapshot={snapshot}/>}
        </aside>
    );
}

function One({row, location}: {row: Row; location: Location | null}) {
    return (
        <>
            <div className="stack">
                <span className="label">{row.folder ? 'Folder' : 'Object'}</span>
                <span className="title">{row.name}</span>
            </div>
            <dl className="kv">
                {!row.folder && <><dt>Size</dt><dd>{formatSize(row.size)}<br/><span className="meta">{formatBytes(row.size)}</span></dd></>}
                {!row.folder && row.modified && <><dt>Modified</dt><dd>{formatDate(row.modified, true)}</dd></>}
                {!row.folder && <><dt>Storage class</dt><dd>{formatStorageClass(row.storageClass) || 'Standard'}</dd></>}
                <dt>Bucket</dt><dd>{location?.bucket}</dd>
                <dt>{row.folder ? 'Prefix' : 'Key'}</dt><dd className="mono">{row.key}</dd>
            </dl>
        </>
    );
}

function Many({info}: {info: SelectionInfo}) {
    const {objects, folders, bytes} = info;
    return (
        <>
            <div className="stack">
                <span className="label">Selection</span>
                <span className="title">{plural(info.count, 'item')} selected</span>
            </div>
            <dl className="kv">
                <dt>Objects</dt><dd>{objects.toLocaleString('en-US')}</dd>
                {folders > 0 && <><dt>Folders</dt><dd>{folders.toLocaleString('en-US')}</dd></>}
                {objects > 0 && <><dt>Total size</dt><dd>{formatSize(bytes)}<br/><span className="meta">{formatBytes(bytes)}{folders > 0 ? ', not counting folders' : ''}</span></dd></>}
            </dl>
        </>
    );
}

function Folder({location, snapshot}: {location: Location | null; snapshot: Snapshot | null}) {
    if (!location) {
        return <p className="meta">Select an object to see its details.</p>;
    }
    const name = location.prefix ? location.prefix.split('/').slice(-2, -1)[0] || '/' : location.bucket;
    return (
        <>
            <div className="stack">
                <span className="label">{location.prefix ? 'This folder' : 'Bucket'}</span>
                <span className="title">{name}</span>
            </div>
            <dl className="kv">
                <dt>Bucket</dt><dd>{location.bucket}</dd>
                <dt>Region</dt><dd>{snapshot?.region ?? location.region ?? 'Detecting…'}</dd>
                {location.prefix && <><dt>Prefix</dt><dd className="mono">{location.prefix}</dd></>}
                {snapshot && <><dt>Contents</dt><dd>{plural(snapshot.objects, 'object')}, {plural(snapshot.folders, 'folder')}{snapshot.state === 'loading' ? ' so far' : ''}</dd></>}
            </dl>
            <p className="meta">Select an object to see its details.</p>
        </>
    );
}
