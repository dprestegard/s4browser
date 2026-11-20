import {useState} from 'react';
import {Dialog} from '../ui/overlay';
import {Icon} from '../ui/Icon';
import {plural} from '../ui/format';
import type {LastSubmit} from './TransferDock';

// What a submit did not queue, one line per file, object or requested item,
// with the reason Go gave. Nothing is ever renamed, so this is the whole story.

// Short labels for the refusal codes of internal/fsx and internal/queue.
const codeLabels: Record<string, string> = {
    absolute: 'Absolute key',
    unc: 'Network path key',
    'drive-path': 'Drive letter in key',
    traversal: '. or .. in key',
    'empty-segment': 'Empty name',
    backslash: 'Backslash',
    'forbidden-char': 'Character Windows forbids',
    'control-char': 'Control character',
    'trailing-dot-space': 'Ends in dot or space',
    'reserved-name': 'Reserved Windows name',
    'name-too-long': 'Name too long',
    'too-long': 'Path too long',
    'invalid-text': 'Not valid text',
    collision: 'Same name as another',
    exists: 'Already exists',
    blocked: 'Blocked',
    link: 'Link or junction',
    relative: 'Not an absolute path',
    'not-found': 'Not found',
    unreadable: 'Cannot be read',
    'not-regular': 'Not a file',
    'key-too-long': 'Key too long',
    duplicate: 'Duplicate',
    'folder-root': 'Drive root',
    'folder-key': 'Folder key with data',
    listing: 'Listing failed',
    stopped: 'Stopped',
};

export function RefusalDialog({last, onClose}: {last: LastSubmit; onClose: () => void}) {
    const {report, verb} = last;
    const refused = report.refused ?? [];
    const more = report.refusedTotal - refused.length;
    const [copied, setCopied] = useState(false);

    const copy = () => {
        const text = refused.map((r) => `${r.path}\t${r.code}\t${r.reason}`).join('\r\n');
        navigator.clipboard?.writeText(text).then(() => setCopied(true), () => setCopied(false));
    };

    return (
        <Dialog title={`${plural(report.refusedTotal, 'item')} not queued`} onClose={onClose} width={760}>
            <p>
                {verb === 'download'
                    ? 'These objects were not downloaded and nothing was written for them. Existing files are never replaced, and names Windows cannot hold are never changed.'
                    : 'These were not uploaded.'}
                {report.queued > 0 ? ` The other ${plural(report.queued, 'file')} ${report.queued === 1 ? 'is' : 'are'} in the queue.` : ''}
            </p>
            <div className="refusals" role="table" aria-label="Items not queued">
                <div className="refusal head" role="row">
                    <span role="columnheader">{verb === 'download' ? 'Object key' : 'Local path'}</span>
                    <span role="columnheader">Why</span>
                </div>
                {refused.map((r, i) => (
                    <div key={i} className="refusal" role="row">
                        <span role="cell" className="mono selectable" style={{overflowWrap: 'anywhere'}}>{r.path || '(the request)'}</span>
                        <span role="cell">
                            <span className="chip warn">{codeLabels[r.code] ?? r.code}</span>
                            <span className="meta selectable" style={{display: 'block', marginTop: 2}}>{r.reason}</span>
                        </span>
                    </div>
                ))}
            </div>
            {more > 0 && <p className="meta">…and {plural(more, 'more item')} for the same kinds of reasons. Only the first {refused.length} are listed.</p>}
            <div className="actions">
                <button type="button" className="btn ghost" style={{marginRight: 'auto'}} onClick={copy}>
                    <Icon name={copied ? 'check' : 'copy'} size="sm"/>{copied ? 'Copied' : 'Copy list'}
                </button>
                <button type="button" className="btn primary" data-autofocus onClick={onClose}>Close</button>
            </div>
        </Dialog>
    );
}
