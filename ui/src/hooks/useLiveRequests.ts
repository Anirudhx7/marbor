import { useState, useEffect, useCallback } from 'react';
import { LiveRequest } from '../types';
import { fetchLiveRequests } from '../lib/api';

const MODELS = [
  'llama3.2:3b',
  'llama3.1:8b',
  'llama3.1:70b',
  'mistral:7b',
  'mixtral:8x7b',
  'qwen2.5:7b',
  'qwen2.5:14b',
  'gemma2:9b',
  'phi3:medium',
  'codellama:13b',
];

const NODES = [
  'gpu-node-01',
  'gpu-node-02',
  'gpu-node-03',
  'gpu-node-04',
];

const API_KEYS = [
  'sk-prod-****-a3f2',
  'sk-prod-****-b7c1',
  'sk-dev-****-d4e5',
  'sk-prod-****-f8g9',
];

function generateRequest(): LiveRequest {
  const model = MODELS[Math.floor(Math.random() * MODELS.length)];
  const isWarm = Math.random() > 0.3;
  
  return {
    id: Math.random().toString(36).substring(2, 15),
    apiKey: API_KEYS[Math.floor(Math.random() * API_KEYS.length)],
    model,
    routedTo: NODES[Math.floor(Math.random() * NODES.length)],
    status: isWarm ? 'warm' : 'loading',
    latency: isWarm ? Math.floor(Math.random() * 150) + 20 : Math.floor(Math.random() * 3000) + 2000,
    timestamp: new Date(),
  };
}

export function useLiveRequests(maxRequests: number = 20) {
  const [requests, setRequests] = useState<LiveRequest[]>([]);
  const [newRequestId, setNewRequestId] = useState<string | null>(null);
  const [isLive, setIsLive] = useState(false);

  const poll = useCallback(async () => {
    try {
      const data = await fetchLiveRequests();
      setRequests(data);
      setIsLive(true);
      if (data.length > 0 && (!requests.length || data[0].id !== requests[0].id)) {
        setNewRequestId(data[0].id);
        setTimeout(() => setNewRequestId(null), 500);
      }
    } catch (e) {
      setIsLive(false);
      const newRequest = generateRequest();
      setNewRequestId(newRequest.id);
      setRequests(prev => [newRequest, ...prev].slice(0, maxRequests));
      setTimeout(() => setNewRequestId(null), 500);
    }
  }, [maxRequests, requests]);

  useEffect(() => {
    const interval = setInterval(poll, 2000);
    return () => clearInterval(interval);
  }, [poll]);

  return { requests, newRequestId, isLive };
}
