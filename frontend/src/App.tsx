import {useCallback, useEffect, useState} from 'react';
import {EventsOn, OnFileDrop} from '../wailsjs/runtime/runtime';
import {Density, ThemeSetting, useAppliedTheme, useSetting} from './settings';
import {useProfiles} from './shell/useProfiles';
import {TopBar} from './shell/TopBar';
import {Sidebar} from './shell/Sidebar';
import {Browser} from './shell/Browser';
import {Details} from './shell/Details';
import {TransferDock} from './shell/TransferDock';
import {Splitter} from './ui/Splitter';
import {Drop, Location, noSelection, SelectionInfo, Snapshot} from './types';

// The app shell approved in stage 1.3: top bar, bucket sidebar, listing,
// details panel and the transfer dock.
export default function App() {
    const [theme, setTheme] = useSetting<ThemeSetting>('theme', 'dark');
    const [density, setDensity] = useSetting<Density>('density', 'compact');
    const [detailsOpen, setDetailsOpen] = useSetting('detailsOpen', true);
    const [sidebarWidth, setSidebarWidth] = useSetting('sidebarWidth', 232);
    const [dockOpen, setDockOpen] = useSetting('dockOpen', true);
    const [dockHeight, setDockHeight] = useSetting('dockHeight', 200);
    useAppliedTheme(theme, density);

    const profiles = useProfiles();
    const profileId = profiles.state.activeId;

    const [location, setLocation] = useState<Location | null>(null);
    const [history, setHistory] = useState<Location[]>([]);
    const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
    const [selection, setSelection] = useState<SelectionInfo>(noSelection);
    const [drops, setDrops] = useState<Drop[]>([]);

    // A new profile starts with nothing open.
    useEffect(() => {
        setLocation(null);
        setHistory([]);
    }, [profileId]);

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

    // Dropped files arrive in Go as absolute paths; Go logs them and sends the
    // paths (never file data) as "s4:drop". The drop target is the listing.
    useEffect(() => {
        OnFileDrop(() => {}, true);
        return EventsOn('s4:drop', (d: Drop) => {
            setDrops((old) => [d, ...old].slice(0, 50));
            setDockOpen(true);
        });
    }, [setDockOpen]);

    // Keep the dock from taking the whole window when the window shrinks.
    const maxDock = Math.max(140, Math.round(window.innerHeight * 0.5));

    return (
        <div className="app">
            <TopBar profiles={profiles} theme={theme} setTheme={setTheme} density={density} setDensity={setDensity}
                    detailsOpen={detailsOpen} toggleDetails={() => setDetailsOpen(!detailsOpen)}/>
            <div className="workspace">
                <Sidebar profileId={profileId} current={location?.bucket ?? ''} width={sidebarWidth}
                         onOpen={(b) => go({bucket: b.name, prefix: '', region: b.region})}/>
                <Splitter dir="v" value={sidebarWidth} min={180} max={360} onChange={setSidebarWidth} label="Resize bucket list"/>
                <Browser profileId={profileId} location={location}
                         onNavigate={(prefix) => location && go({...location, prefix})}
                         canGoBack={history.length > 0} onBack={back}
                         onSnapshot={setSnapshot} onSelection={setSelection} density={density}/>
                {detailsOpen && <Details location={location} snapshot={snapshot} selection={selection}/>}
            </div>
            <div style={{display: 'flex', flexDirection: 'column', minHeight: 0}}>
                {dockOpen && (
                    <Splitter dir="h" value={dockHeight} min={120} max={maxDock} sign={-1} onChange={setDockHeight} label="Resize transfers"/>
                )}
                <TransferDock open={dockOpen} onToggle={() => setDockOpen(!dockOpen)} height={Math.min(dockHeight, maxDock)}
                              drops={drops} onClearDrops={() => setDrops([])}/>
            </div>
        </div>
    );
}
