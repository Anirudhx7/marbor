import { useState, useEffect } from 'react';
import { Plus, Trash2, Server, Thermometer, Cpu, Clock, Activity } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { VramBar } from '../components/VramBar';
import { Badge } from '../components/Badge';
import { Sparkline } from '../components/Sparkline';
import { SearchInput } from '../components/SearchInput';
import { Modal } from '../components/Modal';
import { mockGPUNodes } from '../lib/mockData';
import { fetchNodes, addNode, removeNode } from '../lib/api';
import type { GPUNode } from '../types';

function NodeCard({ node, onRemove }: { node: GPUNode, onRemove: (name: string) => void }) {
  const healthColor = {
    healthy: 'text-primary',
    degraded: 'text-amber-500',
    down: 'text-destructive',
  }[node.health];

  return (
    <div className="bg-card border border-border shadow-sm rounded-xl p-5 hover:border-primary/50 transition-colors">
      {/* Header */}
      <div className="flex items-start justify-between mb-4">
        <div className="flex items-start gap-3">
          <div className="p-2 bg-secondary rounded-lg">
            <Server className="w-5 h-5 text-muted-foreground" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <StatusDot status={node.health} />
              <h3 className="font-semibold text-foreground">{node.name}</h3>
            </div>
            <p className="text-sm text-muted-foreground mt-0.5">{node.gpuModel}</p>
          </div>
        </div>
        <button 
          onClick={() => onRemove(node.name)}
          className="p-1.5 text-muted-foreground hover:text-destructive transition-colors"
        >
          <Trash2 className="w-4 h-4" />
        </button>
      </div>

      {/* Stats Grid */}
      <div className="grid grid-cols-2 gap-4 mb-4">
        <div className="bg-secondary rounded-lg p-3">
          <div className="flex items-center gap-2 text-muted-foreground mb-1">
            <Cpu className="w-3.5 h-3.5" />
            <span className="text-xs font-medium">CPU</span>
          </div>
          <span className={`font-mono text-lg font-medium ${healthColor}`}>
            {node.cpuPercent}%
          </span>
        </div>
        <div className="bg-secondary rounded-lg p-3">
          <div className="flex items-center gap-2 text-muted-foreground mb-1">
            <Thermometer className="w-3.5 h-3.5" />
            <span className="text-xs font-medium">Temp</span>
          </div>
          <span className={`font-mono text-lg font-medium ${
            node.temperature && node.temperature > 80 ? 'text-destructive' : 
            node.temperature && node.temperature > 70 ? 'text-amber-500' : 'text-primary'
          }`}>
            {node.temperature ? `${node.temperature}°C` : 'N/A'}
          </span>
        </div>
      </div>

      {/* VRAM */}
      <div className="mb-4">
        <VramBar used={node.vramUsed} total={node.vramTotal} />
      </div>

      {/* Health History */}
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-2 text-muted-foreground">
          <Activity className="w-3.5 h-3.5" />
          <span className="text-xs font-medium">Health (60m)</span>
        </div>
        <div className="flex items-center gap-2">
          <Sparkline data={node.healthHistory} width={100} height={24} />
          <span className={`text-xs font-mono font-medium ${healthColor}`}>
            {Math.round(node.healthHistory[node.healthHistory.length - 1])}%
          </span>
        </div>
      </div>

      {/* Port & Uptime */}
      <div className="flex items-center justify-between text-xs text-muted-foreground mb-3">
        <div className="flex items-center gap-1.5">
          <Clock className="w-3 h-3" />
          <span className="font-medium">Uptime: {node.uptime}</span>
        </div>
        <code className="font-mono">:{node.port}</code>
      </div>

      {/* Loaded Models */}
      <div className="border-t border-border pt-3">
        <p className="text-xs font-medium text-muted-foreground mb-2">Loaded Models</p>
        <div className="flex flex-wrap gap-1.5">
          {node.loadedModels.map((model) => (
            <Badge 
              key={model.name}
              variant={model.status === 'warm' ? 'success' : 'muted'}
              size="sm"
            >
              {model.name}
              <span className="ml-1.5 opacity-70 font-mono">
                {model.vramUsed.toFixed(1)}GB
              </span>
            </Badge>
          ))}
        </div>
      </div>
    </div>
  );
}

import { useDemoMode } from '../hooks/useDemoMode';

export function GPUNodes() {
  const { demoMode } = useDemoMode();
  const [nodes, setNodes] = useState<GPUNode[]>(demoMode ? mockGPUNodes : []);
  const [isLive, setIsLive] = useState(!demoMode);
  const [searchQuery, setSearchQuery] = useState('');
  const [isAddModalOpen, setIsAddModalOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [newNode, setNewNode] = useState({
    name: '',
    host: '',
    port: '11434',
    gpuModel: '',
  });

  const loadNodes = async () => {
    if (demoMode) {
      setNodes(mockGPUNodes);
      setIsLive(false);
      setError(null);
      return;
    }
    try {
      const data = await fetchNodes();
      setNodes(data || []);
      setIsLive(true);
      setError(null);
    } catch (e: any) {
      setIsLive(false);
      setNodes([]);
      setError(e.message || 'Failed to connect to backend');
    }
  };

  useEffect(() => {
    loadNodes();
    const interval = setInterval(loadNodes, 10000);
    return () => clearInterval(interval);
  }, [demoMode]);

  const filteredNodes = nodes.filter(node =>
    node.name.toLowerCase().includes(searchQuery.toLowerCase()) ||
    node.gpuModel.toLowerCase().includes(searchQuery.toLowerCase())
  );

  const handleAddNode = async () => {
    if (!newNode.name || !newNode.host) return;
    
    const nodeData = {
      name: newNode.name,
      url: `http://${newNode.host}:${newNode.port}`,
      gpu_model: newNode.gpuModel || 'Unknown GPU',
    };

    if (isLive) {
      try {
        await addNode(nodeData);
        loadNodes();
      } catch (e) {
        console.error('Failed to add node');
      }
    } else {
      const node: GPUNode = {
        id: `node-${nodes.length + 1}`,
        name: newNode.name,
        gpuModel: newNode.gpuModel || 'Unknown GPU',
        port: parseInt(newNode.port),
        vramTotal: 24,
        vramUsed: 0,
        cpuPercent: 0,
        temperature: null,
        health: 'healthy',
        uptime: '0m',
        loadedModels: [],
        healthHistory: Array(60).fill(100),
      };
      setNodes([...nodes, node]);
    }
    
    setIsAddModalOpen(false);
    setNewNode({ name: '', host: '', port: '11434', gpuModel: '' });
  };

  const handleRemoveNode = async (name: string) => {
    if (isLive) {
      try {
        await removeNode(name);
        loadNodes();
      } catch (e) {
        console.error('Failed to remove node');
      }
    } else {
      setNodes(nodes.filter(n => n.name !== name));
    }
  };

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground">GPU Nodes</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage {nodes.length} Ollama instances across your infrastructure
          </p>
        </div>
        <div className="flex items-center gap-4">
          <div className="flex items-center gap-2">
            <div className={`w-2 h-2 rounded-full ${isLive ? 'bg-success' : 'bg-amber-500'}`} />
            <span className={`text-xs font-medium ${isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
              {demoMode ? 'Demo Mode' : (isLive ? 'Live Data' : 'Disconnected')}
            </span>
          </div>
          <button
            onClick={() => setIsAddModalOpen(true)}
            className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg transition-colors shadow-sm"
          >
            <Plus className="w-4 h-4" />
            Add Node
          </button>
        </div>
      </div>

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Search */}
      <div className="max-w-md">
        <SearchInput
          value={searchQuery}
          onChange={setSearchQuery}
          placeholder="Search nodes by name or GPU model..."
        />
      </div>

      {/* Nodes Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        {filteredNodes.map((node) => (
          <NodeCard key={node.id} node={node} onRemove={handleRemoveNode} />
        ))}
      </div>

      {filteredNodes.length === 0 && (
        <div className="text-center py-12">
          <Server className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <p className="text-muted-foreground">No GPU nodes found matching your search.</p>
        </div>
      )}

      {/* Add Node Modal */}
      <Modal
        isOpen={isAddModalOpen}
        onClose={() => setIsAddModalOpen(false)}
        title="Add GPU Node"
      >
        <div className="space-y-4">
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Node Name
            </label>
            <input
              type="text"
              value={newNode.name}
              onChange={(e) => setNewNode({ ...newNode, name: e.target.value })}
              placeholder="e.g., gpu-node-05"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Host
            </label>
            <input
              type="text"
              value={newNode.host}
              onChange={(e) => setNewNode({ ...newNode, host: e.target.value })}
              placeholder="e.g., 10.0.1.15"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Port
              </label>
              <input
                type="text"
                value={newNode.port}
                onChange={(e) => setNewNode({ ...newNode, port: e.target.value })}
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                GPU Model
              </label>
              <input
                type="text"
                value={newNode.gpuModel}
                onChange={(e) => setNewNode({ ...newNode, gpuModel: e.target.value })}
                placeholder="e.g., NVIDIA A100"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
          </div>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsAddModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleAddNode}
              disabled={!newNode.name || !newNode.host}
              className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Add Node
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
