import {useEffect, useState} from 'react';
import './App.css';
import {Info} from '../wailsjs/go/main/App';
import {EventsOn, OnFileDrop} from '../wailsjs/runtime/runtime';
import {Profiles} from './Profiles';

// Stage 0.6 spike. Throwaway: it proves that dropping files and folders gives
// absolute paths in Go. It shows path strings and kinds only. File data never
// reaches this page, and Go classifies each path.

type Item = {path: string; kind: string; absolute: boolean; error?: string};
type Drop = {at: string; x: number; y: number; items: Item[]; logError?: string};

function App() {
    const [drops, setDrops] = useState<Drop[]>([]);
    const [logPath, setLogPath] = useState('');
    const [logError, setLogError] = useState('');

    useEffect(() => {
        Info().then((info) => {
            setLogPath(info.logPath);
            setLogError(info.logError ?? '');
        });
        // Installs the window's drag listeners. The paths are resolved and logged
        // in Go, which then sends the result as the "s4:drop" event below.
        OnFileDrop(() => {}, false);
        return EventsOn('s4:drop', (drop: Drop) => setDrops((old) => [drop, ...old]));
    }, []);

    return (
        <div id="App">
            <Profiles/>
            <h1>File-drop spike</h1>
            <p>Drop files, folders or both anywhere in this window.</p>
            <p className="meta">
                {logError ? `Log file unavailable: ${logError}` : `Log file: ${logPath}`}
            </p>
            {drops.length === 0 && <p className="meta">Nothing dropped yet.</p>}
            {drops.map((drop, i) => (
                <section key={drops.length - i} className="drop">
                    <div className="meta">
                        {new Date(drop.at).toLocaleTimeString()}: {drop.items.length} item(s)
                        {drop.logError ? ` (log write failed: ${drop.logError})` : ''}
                    </div>
                    <ul>
                        {drop.items.map((item, j) => (
                            <li key={j}>
                                <span className={`kind kind-${item.kind}`}>{item.kind}</span>{' '}
                                <code>{item.path}</code>
                                {item.absolute ? '' : ' (NOT ABSOLUTE)'}
                                {item.error ? ` (${item.error})` : ''}
                            </li>
                        ))}
                    </ul>
                </section>
            ))}
        </div>
    );
}

export default App
