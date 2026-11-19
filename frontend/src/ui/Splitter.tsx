import {KeyboardEvent, PointerEvent, useRef, useState} from 'react';

// A drag handle between two areas. "v" resizes a width (drag sideways), "h" a
// height (drag up and down). value is the size of the area the handle belongs
// to; sign is +1 when dragging right or down grows it, -1 when it shrinks it.
export function Splitter({dir, value, min, max, sign = 1, onChange, label}: {
    dir: 'v' | 'h';
    value: number;
    min: number;
    max: number;
    sign?: 1 | -1;
    onChange: (v: number) => void;
    label: string;
}) {
    const start = useRef<{pos: number; value: number} | null>(null);
    const [dragging, setDragging] = useState(false);
    const clamp = (v: number) => Math.round(Math.max(min, Math.min(max, v)));

    const down = (e: PointerEvent<HTMLDivElement>) => {
        e.currentTarget.setPointerCapture(e.pointerId);
        start.current = {pos: dir === 'v' ? e.clientX : e.clientY, value};
        setDragging(true);
    };
    const move = (e: PointerEvent<HTMLDivElement>) => {
        if (!start.current) return;
        const pos = dir === 'v' ? e.clientX : e.clientY;
        onChange(clamp(start.current.value + sign * (pos - start.current.pos)));
    };
    const up = () => {
        start.current = null;
        setDragging(false);
    };
    const key = (e: KeyboardEvent) => {
        const step = e.shiftKey ? 40 : 10;
        const grow = dir === 'v' ? {ArrowRight: sign, ArrowLeft: -sign} : {ArrowDown: sign, ArrowUp: -sign};
        const d = grow[e.key as keyof typeof grow];
        if (d) {
            e.preventDefault();
            onChange(clamp(value + d * step));
        }
    };

    return (
        <div className={`splitter ${dir}${dragging ? ' dragging' : ''}`} role="separator" tabIndex={0}
             aria-orientation={dir === 'v' ? 'vertical' : 'horizontal'} aria-label={label}
             aria-valuenow={value} aria-valuemin={min} aria-valuemax={max}
             onPointerDown={down} onPointerMove={move} onPointerUp={up} onPointerCancel={up} onKeyDown={key}/>
    );
}
