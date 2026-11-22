import {useCallback, useEffect, useRef, useState} from 'react';
import {EventsOn, OnFileDrop} from '../wailsjs/runtime/runtime';
import {ChooseDownloadFolder, ChooseUploadFiles, ChooseUploadFolder, StartDownloads, StartUploads} from '../wailsjs/go/app/App';
import {Density, readSavedLocation, saveLocation, ThemeSetting, useAppliedTheme, useSetting} from './settings';
import {useProfiles} from './shell/useProfiles';
import {useQueue} from './shell/useQueue';
import {TopBar} from './shell/TopBar';
import {Sidebar} from './shell/Sidebar';
import {Browser} from './shell/Browser';
import {Details} from './shell/Details';
import {LastSubmit, TransferDock} from './shell/TransferDock';
import {capSummary} from './shell/SettingsDialog';
import {DeleteDialog, NewFolderDialog, PresignDialog} from './shell/ObjectDialogs';
import {Splitter} from './ui/Splitter';
import {Dialog} from './ui/overlay';
import {Icon} from './ui/Icon';
import {formatSize, plural} from './ui/format';
import {Drop, DroppedItem, Location, noSelection, SelectionInfo, SelectionRequest, Snapshot, SubmitReport} from './types';

type PendingDrop = {items: DroppedItem[]; target: Location};

// Uploads that finish into the folder on screen reload its listing on their
// own, at most once per AUTO_RELOAD_GAP and no sooner than AUTO_RELOAD_DELAY
// after the first, so a folder upload of many files does not relist the folder
// for each one. A folder with more rows than AUTO_RELOAD_ROWS is costly to
// relist, so it shows a Reload button instead.
const AUTO_RELOAD_DELAY = 1000;
const AUTO_RELOAD_GAP = 5000;
const AUTO_RELOAD_ROWS = 20000;

// The folder that holds a Windows path, for the next picker to open in.
const parentOf = (p: string) => p.replace(/[\\/]+$/, '').replace(/[\\/][^\\/]*$/, '');

// The app shell approved in stage 1.3: top bar, bucket sidebar, listing,
// details panel and the transfer dock.
export default function App() {
    const [theme, setTheme] = useSetting<ThemeSetting>('theme', 'dark');
    const [density, setDensity] = useSetting<Density>('density', 'compact');
    const [detailsOpen, setDetailsOpen] = useSetting('detailsOpen', true);
    const [sidebarWidth, setSidebarWidth] = useSetting('sidebarWidth', 232);
    const [dockOpen, setDockOpen] = useSetting('dockOpen', true);
    const [dockHeight, setDockHeight] = useSetting('dockHeight', 200);
    const [uploadDir, setUploadDir] = useSetting('lastUploadDir', '');
    const [downloadDir, setDownloadDir] = useSetting('lastDownloadDir', '');
    useAppliedTheme(theme, density);

    const profiles = useProfiles();
    const profileId = profiles.state.activeId;
    const queue = useQueue();

    const [location, setLocation] = useState<Location | null>(null);
    const [history, setHistory] = useState<Location[]>([]);
    const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
    const [selection, setSelection] = useState<SelectionInfo>(noSelection);
    const [busy, setBusy] = useState('');
    const [last, setLast] = useState<LastSubmit | null>(null);
    const [notice, setNotice] = useState('');
    const [pendingDrop, setPendingDrop] = useState<PendingDrop | null>(null);
    const [fresh, setFresh] = useState(0);
    const [refresh, setRefresh] = useState(0);
    const [newFolder, setNewFolder] = useState<Location | null>(null);
    const [deleting, setDeleting] = useState<{at: Location; sel: SelectionRequest} | null>(null);
    const [sharing, setSharing] = useState<{bucket: string; key: string} | null>(null);
    const locationRef = useRef(location);
    locationRef.current = location;

    // A new profile starts with nothing open. After a reload of the window, the
    // location that was open comes back once its profile is the active one.
    const saved = useRef(readSavedLocation());
    const profilesLoaded = profiles.loaded;
    useEffect(() => {
        if (!profilesLoaded) return;
        const s = saved.current;
        saved.current = null;
        if (s && s.profileId === profileId) {
            setLocation({bucket: s.bucket, prefix: s.prefix, region: s.region});
            return;
        }
        setLocation(null);
        setHistory([]);
    }, [profileId, profilesLoaded]);
    useEffect(() => {
        if (!profilesLoaded || saved.current) return;
        saveLocation(location ? {profileId, ...location} : null);
    }, [profileId, profilesLoaded, location]);

    const go = useCallback((next: Location) => {
        if (location && location.bucket === next.bucket && location.prefix === next.prefix) return;
        if (location) setHistory((h) => [...h.slice(-99), location]);
        setLocation(next);
    }, [location]);

    const back = () => {
        const prev = history[history.length - 1];
        if (!prev) return;
        setHistory(history.slice(0, -1));
        setLocation(prev);
    };

    // A listing is a snapshot: count the uploads that finished into the folder on
    // screen since it was listed, so the status bar can offer a reload.
    const snapId = snapshot?.id ?? '';
    useEffect(() => setFresh(0), [snapId, location?.bucket, location?.prefix]);
    useEffect(() => queue.onChange((j, before, initial) => {
        const here = locationRef.current;
        if (initial || j.direction !== 'upload' || j.state !== 'completed' || before?.state === 'completed') return;
        if (here && j.bucket === here.bucket && j.key.startsWith(here.prefix)) setFresh((n) => n + 1);
    }), [queue.onChange]);

    const autoReload = !!snapshot && snapshot.count <= AUTO_RELOAD_ROWS;
    const reloadTimer = useRef<number | null>(null);
    const lastReload = useRef(0);
    useEffect(() => {
        if (fresh === 0 || !autoReload || reloadTimer.current !== null) return;
        const wait = Math.max(AUTO_RELOAD_DELAY, lastReload.current + AUTO_RELOAD_GAP - Date.now());
        reloadTimer.current = window.setTimeout(() => {
            reloadTimer.current = null;
            lastReload.current = Date.now();
            setRefresh((n) => n + 1);
        }, wait);
    }, [fresh, autoReload]);
    // A pending reload belongs to the folder it was planned for.
    useEffect(() => () => {
        if (reloadTimer.current !== null) window.clearTimeout(reloadTimer.current);
        reloadTimer.current = null;
    }, [location?.bucket, location?.prefix, profileId]);

    const report = (verb: 'upload' | 'download', rep: SubmitReport) => {
        setLast({verb, report: rep, at: Date.now()});
        queue.reload();
    };

    const fail = (what: string, err: unknown) => {
        setNotice(`${what}: ${String(err).replace(/^Error: /, '')}`);
        setDockOpen(true);
    };

    const upload = async (paths: string[], target: Location) => {
        if (paths.length === 0) return;
        setDockOpen(true);
        setNotice('');
        setBusy(`Adding ${plural(paths.length, 'item')} to the queue for ${target.bucket}/${target.prefix}… Large folders take a while to read.`);
        try {
            report('upload', await StartUploads({profileId, bucket: target.bucket, region: target.region ?? '', prefix: target.prefix, paths}) as SubmitReport);
        } catch (err) {
            fail('Nothing was uploaded', err);
        } finally {
            setBusy('');
        }
    };

    const pickUpload = async (what: 'files' | 'folder') => {
        const target = locationRef.current;
        if (!target) return;
        try {
            const paths = (await (what === 'files' ? ChooseUploadFiles(uploadDir) : ChooseUploadFolder(uploadDir))) ?? [];
            if (paths.length === 0) return;
            setUploadDir(what === 'files' ? parentOf(paths[0]) : parentOf(paths[0]) || paths[0]);
            await upload(paths, target);
        } catch (err) {
            fail('The file picker failed', err);
        }
    };

    const download = async (sel: SelectionRequest) => {
        const source = locationRef.current;
        if (!source) return;
        try {
            const picked = (await ChooseDownloadFolder(downloadDir)) ?? [];
            if (picked.length === 0) return;
            const dest = picked[0];
            setDownloadDir(dest);
            setDockOpen(true);
            setNotice('');
            const what = sel.folders ? plural(sel.count, 'item') : `${plural(sel.objects, 'object')} (${formatSize(sel.bytes)})`;
            setBusy(`Adding ${what} to the queue, into ${dest}…${sel.folders ? ' Folders are listed first.' : ''}`);
            report('download', await StartDownloads({
                profileId, bucket: source.bucket, region: source.region ?? '', listingId: sel.listingId,
                field: sel.field, desc: sel.desc, version: sel.version, ranges: sel.ranges, destDir: dest,
            } as never) as SubmitReport);
        } catch (err) {
            fail('Nothing was downloaded', err);
        } finally {
            setBusy('');
        }
    };

    // Dropped files arrive in Go as absolute paths; Go logs them and sends the
    // paths (never file data) as "s4:drop". The drop target is the listing. A
    // drop asks before uploading, because a stray drag is easy to make and an
    // upload replaces objects with the same key.
    useEffect(() => {
        OnFileDrop(() => {}, true);
        return EventsOn('s4:drop', (d: Drop) => {
            const target = locationRef.current;
            if (!target) {
                setNotice('Open a bucket, then drop files or folders on the listing to upload them there.');
                setDockOpen(true);
                return;
            }
            if (d.items.length > 0) setPendingDrop({items: d.items, target});
        });
    }, [setDockOpen]);

    const status = queue.status;
    const blocked = !status ? 'Transfers are starting…'
        : !status.available ? `Transfers are not available: ${status.error ?? queue.error}`
        : busy ? 'Wait until the current request is in the queue' : '';
    const uploadBlocked = blocked || (status?.uploadsDisabled ? `Uploads are turned off: ${status.uploadsDisabledReason ?? ''}` : '');

    // Keep the dock from taking the whole window when the window shrinks.
    const maxDock = Math.max(140, Math.round(window.innerHeight * 0.5));

    return (
        <div className="app">
            <TopBar profiles={profiles} theme={theme} setTheme={setTheme} density={density} setDensity={setDensity}
                    detailsOpen={detailsOpen} toggleDetails={() => setDetailsOpen(!detailsOpen)}
                    bandwidth={capSummary(queue.settings?.current)}/>
            <div className="workspace">
                <Sidebar profileId={profileId} current={location?.bucket ?? ''} width={sidebarWidth}
                         onOpen={(b) => go({bucket: b.name, prefix: '', region: b.region})}/>
                <Splitter dir="v" value={sidebarWidth} min={180} max={360} onChange={setSidebarWidth} label="Resize bucket list"/>
                <Browser profileId={profileId} location={location}
                         onNavigate={(prefix) => location && go({...location, prefix})}
                         canGoBack={history.length > 0} onBack={back}
                         onSnapshot={setSnapshot} onSelection={setSelection} density={density}
                         onUpload={pickUpload} onDownload={download}
                         uploadBlocked={uploadBlocked} downloadBlocked={blocked} freshUploads={autoReload ? 0 : fresh}
                         onNewFolder={() => location && setNewFolder(location)}
                         onDelete={(sel) => location && setDeleting({at: location, sel})} refresh={refresh}/>
                {detailsOpen && <Details profileId={profileId} location={location} snapshot={snapshot} selection={selection}
                                         onShare={(key) => location && setSharing({bucket: location.bucket, key})}/>}
            </div>
            <div style={{display: 'flex', flexDirection: 'column', minHeight: 0}}>
                {dockOpen && (
                    <Splitter dir="h" value={dockHeight} min={120} max={maxDock} sign={-1} onChange={setDockHeight} label="Resize transfers"/>
                )}
                <TransferDock open={dockOpen} onToggle={() => setDockOpen(!dockOpen)} height={Math.min(dockHeight, maxDock)}
                              queue={queue} busy={busy} last={last} onDismissLast={() => setLast(null)}
                              notice={notice} onDismissNotice={() => setNotice('')}/>
            </div>
            {newFolder && (
                <NewFolderDialog profileId={profileId} bucket={newFolder.bucket} prefix={newFolder.prefix}
                                 onClose={() => setNewFolder(null)}
                                 onCreated={() => { setNewFolder(null); setRefresh((n) => n + 1); }}/>
            )}
            {deleting && (
                <DeleteDialog profileId={profileId} bucket={deleting.at.bucket} sel={deleting.sel}
                              onClose={() => setDeleting(null)} onDeleted={() => setRefresh((n) => n + 1)}/>
            )}
            {sharing && (
                <PresignDialog profileId={profileId} bucket={sharing.bucket} objectKey={sharing.key} onClose={() => setSharing(null)}/>
            )}
            {pendingDrop && (
                <DropDialog drop={pendingDrop} blocked={uploadBlocked} onClose={() => setPendingDrop(null)} onUpload={() => {
                    const d = pendingDrop;
                    setPendingDrop(null);
                    void upload(d.items.map((it) => it.path), d.target);
                }}/>
            )}
        </div>
    );
}

// Confirms a drop: what was dropped and where it goes.
function DropDialog({drop, blocked, onClose, onUpload}: {drop: PendingDrop; blocked: string; onClose: () => void; onUpload: () => void}) {
    const {items, target} = drop;
    const shown = items.slice(0, 6);
    const folders = items.filter((it) => it.kind === 'folder').length;
    return (
        <Dialog title={`Upload ${plural(items.length, 'item')}?`} onClose={onClose} width={520}>
            <div className="stack">
                {shown.map((it) => (
                    <div key={it.path} style={{display: 'flex', alignItems: 'center', gap: 8, minWidth: 0}}>
                        <Icon name={it.kind === 'folder' ? 'folder' : 'file'} className={it.kind === 'folder' ? 'folder-icon' : 'file-icon'}/>
                        <span style={{flex: 'none', maxWidth: '50%'}} className="ellipsis" title={it.path}>{it.path.split(/[\\/]/).pop() || it.path}</span>
                        <span className="ellipsis meta mono" title={it.path}>{parentOf(it.path)}</span>
                        {it.error && <span className="chip warn">{it.kind === 'missing' ? 'Not found' : 'Cannot read'}</span>}
                    </div>
                ))}
                {items.length > shown.length && <span className="meta">…and {plural(items.length - shown.length, 'more item')}</span>}
            </div>
            <p>
                To <span className="mono" style={{color: 'var(--text)'}}>{target.bucket}/{target.prefix}</span>.
                {folders > 0 ? ' Folders upload with everything inside them.' : ''} An object that already has the same key is replaced.
            </p>
            {blocked && <p className="meta t-warn" role="alert">{blocked}</p>}
            <div className="actions">
                <button type="button" className="btn" onClick={onClose}>Cancel</button>
                <button type="button" className="btn primary" data-autofocus disabled={!!blocked} onClick={onUpload}>
                    <Icon name="upload"/>Upload
                </button>
            </div>
        </Dialog>
    );
}
