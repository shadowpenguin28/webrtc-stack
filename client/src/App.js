import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import './App.css';

const defaultSignalUrl = () => {
  const fromEnvironment = process.env.REACT_APP_SIGNALING_URL;
  if (fromEnvironment) return fromEnvironment;
  const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
  return `${scheme}://${window.location.hostname}:8080/ws`;
};

const createSessionId = () => (
  window.crypto?.randomUUID?.() || `viewer-${Date.now()}-${Math.random().toString(16).slice(2)}`
);

const persistentViewerId = () => {
  const storageKey = 'rover-base-station.viewer-id';
  const existing = window.localStorage.getItem(storageKey);
  if (existing) return existing;
  const viewerId = createSessionId();
  window.localStorage.setItem(storageKey, viewerId);
  return viewerId;
};

const newConnection = (nodeId) => ({
  nodeId,
  sessionId: createSessionId(),
  status: 'connecting',
  detail: 'Creating WebRTC offer',
  stream: null,
  pc: null,
  queuedCandidates: [],
  stats: { bitrate: '—', fps: '—', loss: '—', rtt: '—' },
});

function App() {
  const [signalUrl, setSignalUrl] = useState(() => new URLSearchParams(window.location.search).get('signal') || defaultSignalUrl());
  const [draftSignalUrl, setDraftSignalUrl] = useState(signalUrl);
  const [isEditingSignalUrl, setIsEditingSignalUrl] = useState(false);
  const [signalStatus, setSignalStatus] = useState('connecting');
  const [nodes, setNodes] = useState([]);
  const [manualNodeId, setManualNodeId] = useState('simulated-pi-a');
  const [connections, setConnections] = useState({});
  const [events, setEvents] = useState([]);
  const socketRef = useRef(null);
  const connectionsRef = useRef({});
  const viewerIdRef = useRef(persistentViewerId());

  const log = useCallback((message) => {
    const stamp = new Date().toLocaleTimeString();
    setEvents((current) => [`${stamp}  ${message}`, ...current].slice(0, 80));
  }, []);

  const send = useCallback((message) => {
    const socket = socketRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      log(`Cannot send ${message.type}: signaling socket is not connected`);
      return false;
    }
    socket.send(JSON.stringify(message));
    return true;
  }, [log]);

  const updateConnection = useCallback((nodeId, change) => {
    const current = connectionsRef.current[nodeId];
    if (!current) return;
    const updated = { ...current, ...change };
    connectionsRef.current = { ...connectionsRef.current, [nodeId]: updated };
    setConnections(connectionsRef.current);
  }, []);

  const stopStream = useCallback((nodeId, reason = 'Stopped by operator') => {
    const connection = connectionsRef.current[nodeId];
    if (!connection) return;
    send({ type: 'disconnect', nodeId, sessionId: connection.sessionId });
    connection.pc?.close();
    const next = { ...connectionsRef.current };
    delete next[nodeId];
    connectionsRef.current = next;
    setConnections(next);
    log(`${nodeId}: ${reason}`);
  }, [log, send]);

  const applyRemoteCandidate = useCallback(async (nodeId, candidate) => {
    const connection = connectionsRef.current[nodeId];
    if (!connection || !candidate) return;
    if (!connection.pc.remoteDescription) {
      connection.queuedCandidates.push(candidate);
      return;
    }
    try {
      await connection.pc.addIceCandidate(candidate);
    } catch (error) {
      log(`${nodeId}: could not add ICE candidate: ${error.message}`);
    }
  }, [log]);

  const handleSignalMessage = useCallback(async (message) => {
    switch (message.type) {
      case 'nodes':
        setNodes(Array.isArray(message.nodes) ? message.nodes : []);
        log(`Received ${message.nodes?.length || 0} registered node(s)`);
        break;
      case 'node-status':
        setNodes((current) => {
          const exists = current.some((node) => node.nodeId === message.nodeId);
          const node = { nodeId: message.nodeId, ...message.node };
          return exists ? current.map((item) => item.nodeId === message.nodeId ? { ...item, ...node } : item) : [...current, node];
        });
        break;
      case 'answer': {
        const connection = connectionsRef.current[message.nodeId];
        if (!connection || connection.sessionId !== message.sessionId) break;
        try {
          await connection.pc.setRemoteDescription(message.payload);
          for (const candidate of connection.queuedCandidates.splice(0)) {
            await connection.pc.addIceCandidate(candidate);
          }
          updateConnection(message.nodeId, { detail: 'Answer received; checking direct UDP path' });
          log(`${message.nodeId}: answer applied`);
        } catch (error) {
          updateConnection(message.nodeId, { status: 'failed', detail: error.message });
          log(`${message.nodeId}: invalid answer: ${error.message}`);
        }
        break;
      }
      case 'ice-candidate':
        await applyRemoteCandidate(message.nodeId, message.payload);
        break;
      case 'error':
        log(`Signaling error${message.nodeId ? ` (${message.nodeId})` : ''}: ${message.message || 'unknown error'}`);
        if (message.nodeId) updateConnection(message.nodeId, { status: 'failed', detail: message.message || 'Signaling error' });
        break;
      default:
        log(`Ignoring unknown signaling message: ${message.type || 'missing type'}`);
    }
  }, [applyRemoteCandidate, log, updateConnection]);

  useEffect(() => {
    if (isEditingSignalUrl) {
      setSignalStatus('paused');
      return;
    }

    let reconnectTimer;
    let disposed = false;
    const connect = () => {
      setSignalStatus('connecting');
      const socket = new WebSocket(signalUrl);
      socketRef.current = socket;
      socket.onopen = () => {
        setSignalStatus('connected');
        log(`Signaling connected: ${signalUrl}`);
        socket.send(JSON.stringify({ type: 'register-viewer', viewerId: viewerIdRef.current }));
        socket.send(JSON.stringify({ type: 'list-nodes' }));
      };
      socket.onmessage = (event) => {
        try { handleSignalMessage(JSON.parse(event.data)); }
        catch (error) { log(`Invalid signaling message: ${error.message}`); }
      };
      socket.onerror = () => log('Signaling WebSocket encountered an error');
      socket.onclose = () => {
        if (socketRef.current === socket) socketRef.current = null;
        setSignalStatus('disconnected');
        if (!disposed) reconnectTimer = window.setTimeout(connect, 2000);
      };
    };
    connect();
    return () => {
      disposed = true;
      window.clearTimeout(reconnectTimer);
      socketRef.current?.close();
    };
  }, [handleSignalMessage, log, signalUrl, isEditingSignalUrl]);

  useEffect(() => () => {
    Object.values(connectionsRef.current).forEach((connection) => connection.pc?.close());
  }, []);

  const startStream = useCallback(async (rawNodeId) => {
    const nodeId = rawNodeId.trim();
    if (!nodeId || connectionsRef.current[nodeId]) return;
    const connection = newConnection(nodeId);
    const pc = new RTCPeerConnection({ iceServers: [] });
    connection.pc = pc;
    connectionsRef.current = { ...connectionsRef.current, [nodeId]: connection };
    setConnections(connectionsRef.current);

    pc.ontrack = (event) => {
      updateConnection(nodeId, { stream: event.streams[0], detail: 'Receiving video' });
      log(`${nodeId}: received remote video track`);
    };
    pc.onicecandidate = ({ candidate }) => {
      if (candidate) send({ type: 'ice-candidate', nodeId, sessionId: connection.sessionId, payload: candidate });
    };
    pc.oniceconnectionstatechange = () => {
      updateConnection(nodeId, { detail: `ICE: ${pc.iceConnectionState}` });
      log(`${nodeId}: ICE ${pc.iceConnectionState}`);
    };
    pc.onconnectionstatechange = () => {
      const state = pc.connectionState;
      updateConnection(nodeId, { status: state === 'failed' || state === 'closed' ? 'failed' : state, detail: `WebRTC: ${state}` });
      log(`${nodeId}: WebRTC ${state}`);
    };
    try {
      const transceiver = pc.addTransceiver('video', { direction: 'recvonly' });
      // Pin H.264 — the rover only sends H.264, so reject VP8/VP9 upfront
      const h264Codecs = RTCRtpReceiver.getCapabilities?.('video')?.codecs
        ?.filter(c => c.mimeType === 'video/H264') || [];
      if (h264Codecs.length > 0 && transceiver.setCodecPreferences) {
        transceiver.setCodecPreferences(h264Codecs);
      }
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      if (!send({ type: 'offer', nodeId, sessionId: connection.sessionId, payload: pc.localDescription })) {
        updateConnection(nodeId, { status: 'failed', detail: 'Signaling server is unavailable' });
      }
    } catch (error) {
      updateConnection(nodeId, { status: 'failed', detail: error.message });
      log(`${nodeId}: could not start WebRTC: ${error.message}`);
    }
  }, [log, send, updateConnection]);

  useEffect(() => {
    const interval = window.setInterval(async () => {
      for (const connection of Object.values(connectionsRef.current)) {
        if (!connection.pc) continue;
        const reports = await connection.pc.getStats();
        let inbound; let pair;
        reports.forEach((report) => {
          if (report.type === 'inbound-rtp' && report.kind === 'video') inbound = report;
          if (report.type === 'candidate-pair' && (report.state === 'succeeded' || report.state === 'in-progress')) {
            // Prefer nominated pairs, but accept any active pair for RTT
            if (!pair || report.nominated) pair = report;
          }
        });
        if (!inbound) continue;
        const previous = connection.lastInbound;
        const seconds = previous ? (inbound.timestamp - previous.timestamp) / 1000 : 0;
        const bitrate = previous && seconds > 0 ? `${Math.round(((inbound.bytesReceived - previous.bytesReceived) * 8) / seconds / 1000)} kb/s` : 'measuring';
        // Compute FPS from framesDecoded delta — more reliable than framesPerSecond
        const fps = previous && seconds > 0 && inbound.framesDecoded != null
          ? Math.round((inbound.framesDecoded - (previous.framesDecoded || 0)) / seconds)
          : inbound.framesPerSecond ?? '—';
        connection.lastInbound = { bytesReceived: inbound.bytesReceived, framesDecoded: inbound.framesDecoded, timestamp: inbound.timestamp };
        updateConnection(connection.nodeId, { stats: {
          bitrate, fps, loss: `${inbound.packetsLost ?? 0} packets`,
          rtt: pair?.currentRoundTripTime ? `${Math.round(pair.currentRoundTripTime * 1000)} ms` : '—',
        } });
      }
    }, 1000);
    return () => window.clearInterval(interval);
  }, [updateConnection]);

  const nodeCards = useMemo(() => {
    const known = new Map(nodes.map((node) => [node.nodeId, node]));
    Object.keys(connections).forEach((nodeId) => {
      if (!known.has(nodeId)) known.set(nodeId, { nodeId, label: nodeId });
    });
    return [...known.values()];
  }, [connections, nodes]);

  return (
    <main className="app-shell">
      <header className="topbar"><div><p className="eyebrow">ROVER BASE STATION</p><h1>Video monitor</h1></div><span className={`signal-status ${signalStatus}`}>Signaling: {signalStatus}</span></header>
      <section className="connection-panel">
        <label>
          Signaling WebSocket URL
          <span style={{ display: 'flex', gap: '8px' }}>
            <input 
              value={isEditingSignalUrl ? draftSignalUrl : signalUrl} 
              onChange={(event) => setDraftSignalUrl(event.target.value)} 
              disabled={!isEditingSignalUrl}
              spellCheck="false" 
              style={{ flex: 1 }}
            />
            {isEditingSignalUrl ? (
              <button type="button" onClick={() => { setSignalUrl(draftSignalUrl); setIsEditingSignalUrl(false); }}>Confirm</button>
            ) : (
              <button type="button" onClick={() => { setDraftSignalUrl(signalUrl); setIsEditingSignalUrl(true); }}>Edit</button>
            )}
          </span>
        </label>
        <label>Rover node ID<input value={manualNodeId} onChange={(event) => setManualNodeId(event.target.value)} spellCheck="false" /></label>
        <button type="button" onClick={() => startStream(manualNodeId)} disabled={signalStatus !== 'connected'}>Connect stream</button>
      </section>
      <section className="stream-grid" aria-label="Rover video streams">
        {nodeCards.length === 0 && <p className="empty-state">No rover nodes have registered yet. You can enter a node ID above to test signaling.</p>}
        {nodeCards.map((node) => {
          const connection = connections[node.nodeId];
          return <article className="stream-card" key={node.nodeId}>
            <div className="stream-heading"><div><h2>{node.label || node.nodeId}</h2><p>{node.nodeId}</p></div><span className={`stream-status ${connection?.status || node.status || 'offline'}`}>{connection?.status || node.status || 'offline'}</span></div>
            <div className="video-frame">{connection?.stream ? <video autoPlay playsInline muted ref={(element) => { if (element && element.srcObject !== connection.stream) element.srcObject = connection.stream; }} /> : <p>{connection?.detail || 'Select Connect stream to request video.'}</p>}</div>
            <dl className="stats"><div><dt>Bitrate</dt><dd>{connection?.stats.bitrate || '—'}</dd></div><div><dt>FPS</dt><dd>{connection?.stats.fps || '—'}</dd></div><div><dt>Loss</dt><dd>{connection?.stats.loss || '—'}</dd></div><div><dt>RTT</dt><dd>{connection?.stats.rtt || '—'}</dd></div></dl>
            {connection ? <button className="secondary" type="button" onClick={() => stopStream(node.nodeId)}>Disconnect</button> : <button className="secondary" type="button" onClick={() => startStream(node.nodeId)} disabled={signalStatus !== 'connected'}>Connect</button>}
          </article>;
        })}
      </section>
      <section className="event-panel"><h2>Operator log</h2><pre>{events.length ? events.join('\n') : 'Waiting for signaling connection…'}</pre></section>
    </main>
  );
}

export default App;
