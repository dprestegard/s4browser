import {ReactNode, RefObject, useEffect, useRef} from 'react';

// Closes a menu on Escape or on a pointer press outside it (and outside its
// opening button, which toggles it itself).
export function useDismiss(open: boolean, onClose: () => void, refs: RefObject<HTMLElement | null>[]) {
    useEffect(() => {
        if (!open) return;
        const down = (e: PointerEvent) => {
            if (refs.some((r) => r.current?.contains(e.target as Node))) return;
            onClose();
        };
        const key = (e: KeyboardEvent) => {
            if (e.key === 'Escape') onClose();
        };
        document.addEventListener('pointerdown', down);
        document.addEventListener('keydown', key);
        return () => {
            document.removeEventListener('pointerdown', down);
            document.removeEventListener('keydown', key);
        };
        // refs are stable objects
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [open, onClose]);
}

const focusable = 'button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea, [href], [tabindex]:not([tabindex="-1"])';

// A modal dialog. Focus moves into it (to the element marked data-autofocus, or
// the first control), Tab stays inside, Escape closes, and focus returns to
// where it was.
export function Dialog({title, onClose, children, role = 'dialog', width}: {
    title: string;
    onClose: () => void;
    children: ReactNode;
    role?: 'dialog' | 'alertdialog';
    width?: number;
}) {
    const ref = useRef<HTMLDivElement>(null);
    const titleId = useRef('dlg-' + Math.random().toString(36).slice(2)).current;

    useEffect(() => {
        const previous = document.activeElement as HTMLElement | null;
        const el = ref.current;
        const first = el?.querySelector<HTMLElement>('[data-autofocus]') ?? el?.querySelector<HTMLElement>(focusable);
        first?.focus();
        return () => previous?.focus?.();
    }, []);

    const onKeyDown = (e: React.KeyboardEvent) => {
        if (e.key === 'Escape') {
            e.stopPropagation();
            onClose();
            return;
        }
        if (e.key !== 'Tab' || !ref.current) return;
        const items = Array.from(ref.current.querySelectorAll<HTMLElement>(focusable));
        if (items.length === 0) return;
        const first = items[0];
        const last = items[items.length - 1];
        if (e.shiftKey && document.activeElement === first) {
            e.preventDefault();
            last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
            e.preventDefault();
            first.focus();
        }
    };

    return (
        <div className="scrim" onPointerDown={(e) => e.target === e.currentTarget && onClose()}>
            <div ref={ref} className="dialog" role={role} aria-modal="true" aria-labelledby={titleId}
                 onKeyDown={onKeyDown} style={width ? {width} : undefined}>
                <h2 id={titleId}>{title}</h2>
                {children}
            </div>
        </div>
    );
}
