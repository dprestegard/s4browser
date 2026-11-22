import React from 'react'
import {createRoot} from 'react-dom/client'
import './styles/tokens.css'
import './styles/base.css'
import './styles/shell.css'
import {applyInitialTheme} from './settings'
import App from './App'
import {isMac} from './ui/platform'

applyInitialTheme()
if (isMac) document.documentElement.dataset.platform = 'mac'

const root = createRoot(document.getElementById('root')!)

root.render(
    <React.StrictMode>
        <App/>
    </React.StrictMode>
)
