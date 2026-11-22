import {Events} from '@wailsio/runtime';

// EventsOn subscribes to an event the Go side emits and passes the handler the
// payload alone. It returns the function that unsubscribes.
export function EventsOn<T = any>(name: string, handler: (payload: T) => void): () => void {
    return Events.On(name, (ev) => handler(ev.data as T));
}
