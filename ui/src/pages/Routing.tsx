import { useState, useEffect } from 'react';
import { 
  Zap, 
  Clock, 
  ArrowUpRight, 
  Plus, 
  Trash2, 
  Check, 
  Route,
  Shield,
  Server
} from 'lucide-react';
import { Badge } from '../components/Badge';
import { Modal } from '../components/Modal';
import { mockGPUNodes } from '../lib/mockData';
import {
  fetchRoutingRules,
  addRoutingRule,
  removeRoutingRule,
  toggleRoutingRule,
  setRoutingStrategy,
  fetchRoutingStrategy,
  fetchNodes
} from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';

const STRATEGIES = [
  { 
    value: 'warm-first', 
    label: 'Warm First', 
    description: 'Prioritize nodes that already have the model loaded in VRAM.',
    icon: <Zap className="w-4 h-4" />
  },
  { 
    value: 'least-connections', 
    label: 'Least Connections', 
    description: 'Route to the node with the fewest active requests.',
    icon: <ArrowUpRight className="w-4 h-4" />
  },
  { 
    value: 'round-robin', 
    label: 'Round Robin', 
    description: 'Cycle through all healthy nodes sequentially.',
    icon: <Route className="w-4 h-4" />
  },
];

interface RoutingRule {
  id: string;
  priority: number;
  condition: string;
  targetNode: string;
  strategy: string;
  enabled: boolean;
}

const MOCK_RULES: RoutingRule[] = [
  {
    id: '1',
    priority: 10,
    condition: 'model =~ "70b"',
    targetNode: 'gpu-node-01',
    strategy: 'warm-first',
    enabled: true,
  },
  {
    id: '2',
    priority: 20,
    condition: 'api_key == "sk-prod-*"',
    targetNode: 'any',
    strategy: 'least-connections',
    enabled: true,
  },
];

export function Routing() {
  const { demoMode } = useDemoMode();
  const [currentStrategy, setCurrentStrategyState] = useState('');
  const [rules, setRules] = useState<RoutingRule[]>(demoMode ? MOCK_RULES : []);
  const [availableNodes, setAvailableNodes] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [isCreateModalOpen, setIsCreateModalOpen] = useState(false);
  const [isDeleteModalOpen, setIsDeleteModalOpen] = useState(false);
  const [ruleToDelete, setRuleToDelete] = useState<RoutingRule | null>(null);
  const [error, setError] = useState<string | null>(null);
  
  const [newRuleForm, setNewRuleForm] = useState({
    priority: '',
    condition: '',
    targetNode: '',
    strategy: 'warm-first',
  });
  const [formErrors, setFormErrors] = useState<string[]>([]);

  const loadData = async () => {
    if (demoMode) {
      setRules(MOCK_RULES);
      setAvailableNodes(mockGPUNodes);
      setCurrentStrategyState('warm-first');
      setError(null);
      setLoading(false);
      return;
    }

    try {
      const [rulesData, nodesData] = await Promise.all([
        fetchRoutingRules(),
        fetchNodes(),
      ]);
      setRules(Array.isArray(rulesData) ? rulesData : []);
      setAvailableNodes(nodesData || []);
      setError(null);
      // Fetch strategy separately so failure is visible to the user
      try {
        const strategy = await fetchRoutingStrategy();
        setCurrentStrategyState(strategy);
      } catch {
        setCurrentStrategyState('');
      }
    } catch (err: any) {
      setError(err.message || 'Failed to connect to backend');
      setRules([]);
      setAvailableNodes([]);
      setCurrentStrategyState('');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadData();
  }, [demoMode]);

  const handleStrategyChange = async (strategy: string) => {
    if (demoMode) {
      setCurrentStrategyState(strategy);
      return;
    }
    
    try {
      await setRoutingStrategy(strategy);
      setCurrentStrategyState(strategy);
      setError(null);
    } catch (err: any) {
      setError(err.message);
    }
  };

  const handleToggleRule = async (id: string) => {
    if (demoMode) {
      setRules(rules.map(r => r.id === id ? { ...r, enabled: !r.enabled } : r));
      return;
    }

    try {
      await toggleRoutingRule(id);
      await loadData();
    } catch (err: any) {
      setError(err.message);
    }
  };

  const handleCreateRule = async () => {
    const errors: string[] = [];
    if (!newRuleForm.priority) errors.push('Priority is required');
    if (!newRuleForm.condition) errors.push('Condition is required');
    if (!newRuleForm.targetNode) errors.push('Target node is required');
    
    if (errors.length > 0) {
      setFormErrors(errors);
      return;
    }

    const newRule: RoutingRule = {
      id: `rule-${Date.now()}`,
      priority: parseInt(newRuleForm.priority),
      condition: newRuleForm.condition,
      targetNode: newRuleForm.targetNode,
      strategy: newRuleForm.strategy,
      enabled: true,
    };

    if (demoMode) {
      setRules([...rules, newRule].sort((a, b) => a.priority - b.priority));
    } else {
      try {
        await addRoutingRule(newRule);
        await loadData();
      } catch (err: any) {
        setFormErrors([err.message]);
        return;
      }
    }

    setIsCreateModalOpen(false);
    setNewRuleForm({ priority: '', condition: '', targetNode: '', strategy: 'warm-first' });
    setFormErrors([]);
  };

  const handleDeleteRule = async () => {
    if (!ruleToDelete) return;
    
    if (demoMode) {
      setRules(rules.filter(r => r.id !== ruleToDelete.id));
    } else {
      try {
        await removeRoutingRule(ruleToDelete.id);
        await loadData();
      } catch (err: any) {
        setError(err.message);
      }
    }
    
    setIsDeleteModalOpen(false);
    setRuleToDelete(null);
  };

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 border-b border-border pb-6">
        <div>
          <h1 className="text-2xl font-bold text-foreground">Routing Logic</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Configure how requests are balanced across your cluster
          </p>
        </div>
      </div>

      {error && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Global Strategy Selection */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        {STRATEGIES.map((strategy) => (
          <button
            key={strategy.value}
            onClick={() => handleStrategyChange(strategy.value)}
            className={`flex flex-col p-5 rounded-xl border text-left transition-colors ${
              currentStrategy === strategy.value
                ? 'bg-primary/5 border-primary shadow-sm'
                : 'bg-card border-border hover:border-border/80 shadow-sm'
            }`}
          >
            <div className={`p-2 rounded-lg mb-4 w-fit ${
              currentStrategy === strategy.value ? 'bg-primary text-primary-foreground' : 'bg-secondary text-muted-foreground'
            }`}>
              {strategy.icon}
            </div>
            <h3 className="font-semibold text-foreground mb-1">{strategy.label}</h3>
            <p className="text-sm text-muted-foreground leading-relaxed">
              {strategy.description}
            </p>
            {currentStrategy === strategy.value && (
              <div className="mt-4 flex items-center gap-1.5 text-xs text-primary font-medium">
                <Check className="w-4 h-4" />
                Active Strategy
              </div>
            )}
          </button>
        ))}
      </div>

      {!demoMode && !loading && currentStrategy === '' && (
        <div className="p-3 bg-amber-500/10 border border-amber-500/30 rounded-xl text-amber-600 dark:text-amber-400 text-sm font-medium">
          Could not read strategy from backend - no strategy is currently selected
        </div>
      )}

      {/* Advanced Rules Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 pt-6">
        <div>
          <h2 className="text-lg font-semibold text-foreground">Override Rules</h2>
          <p className="text-sm text-muted-foreground">Fine-grained control for specific models or API keys</p>
        </div>
        <button
          onClick={() => setIsCreateModalOpen(true)}
          className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm self-start sm:self-auto"
        >
          <Plus className="w-4 h-4" />
          Add Rule
        </button>
      </div>

      {/* Rules Table */}
      <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-secondary/50 border-b border-border text-muted-foreground">
                <th className="px-6 py-3 text-left font-medium">Priority</th>
                <th className="px-6 py-3 text-left font-medium">Condition</th>
                <th className="px-6 py-3 text-left font-medium">Target</th>
                <th className="px-6 py-3 text-left font-medium">Strategy</th>
                <th className="px-6 py-3 text-center font-medium">Status</th>
                <th className="px-6 py-3 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rules.map((rule) => (
                <tr key={rule.id} className={`${rule.enabled ? 'opacity-100' : 'opacity-50'} hover:bg-secondary/30 transition-colors`}>
                  <td className="px-6 py-4">
                    <span className="font-mono font-medium text-muted-foreground">#{rule.priority}</span>
                  </td>
                  <td className="px-6 py-4">
                    <code className="text-xs px-2 py-1 bg-secondary rounded-md border border-border font-mono">
                      {rule.condition}
                    </code>
                  </td>
                  <td className="px-6 py-4">
                    <div className="flex items-center gap-2">
                      <Server className="w-4 h-4 text-muted-foreground" />
                      <span className="font-medium text-foreground">{rule.targetNode}</span>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    <Badge variant="primary" size="sm">
                      {STRATEGIES.find(s => s.value === rule.strategy)?.label || rule.strategy}
                    </Badge>
                  </td>
                  <td className="px-6 py-4 text-center">
                    <button
                      onClick={() => handleToggleRule(rule.id)}
                      className={`inline-flex items-center justify-center w-8 h-8 rounded-lg transition-colors ${
                        rule.enabled 
                          ? 'bg-primary/10 text-primary' 
                          : 'bg-muted text-muted-foreground'
                      }`}
                    >
                      {rule.enabled ? <Check className="w-4 h-4" /> : <span className="text-xs font-bold">✕</span>}
                    </button>
                  </td>
                  <td className="px-6 py-4 text-right">
                    <button
                      onClick={() => {
                        setRuleToDelete(rule);
                        setIsDeleteModalOpen(true);
                      }}
                      className="p-2 text-muted-foreground hover:text-destructive transition-colors"
                    >
                      <Trash2 className="w-4 h-4" />
                    </button>
                  </td>
                </tr>
              ))}
              {loading && rules.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-6 py-8 text-center text-muted-foreground">
                    Loading...
                  </td>
                </tr>
              )}
              {!loading && rules.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-6 py-8 text-center text-muted-foreground">
                    No override rules configured.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      {/* Create Rule Modal */}
      <Modal
        isOpen={isCreateModalOpen}
        onClose={() => {
          setIsCreateModalOpen(false);
          setFormErrors([]);
          setNewRuleForm({ priority: '', condition: '', targetNode: '', strategy: 'warm-first' });
        }}
        title="Add Routing Rule"
      >
        <div className="space-y-4">
          {formErrors.length > 0 && (
            <div className="p-3 bg-destructive/10 border border-destructive/20 rounded-lg">
              {formErrors.map((error, i) => (
                <p key={i} className="text-sm font-medium text-destructive">{error}</p>
              ))}
            </div>
          )}
          
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Priority <span className="text-destructive">*</span>
              </label>
              <input
                type="number"
                value={newRuleForm.priority}
                onChange={(e) => setNewRuleForm({ ...newRuleForm, priority: e.target.value })}
                placeholder="1"
                min="1"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Target Node <span className="text-destructive">*</span>
              </label>
              <select
                value={newRuleForm.targetNode}
                onChange={(e) => setNewRuleForm({ ...newRuleForm, targetNode: e.target.value })}
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
              >
                <option value="">Select node...</option>
                <option value="any">Any Node (Dynamic Balancing)</option>
                {availableNodes.map((node) => (
                  <option key={node.id} value={node.name}>{node.name}</option>
                ))}
              </select>
            </div>
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Condition <span className="text-destructive">*</span>
            </label>
            <input
              type="text"
              value={newRuleForm.condition}
              onChange={(e) => setNewRuleForm({ ...newRuleForm, condition: e.target.value })}
              placeholder='e.g., model =~ "70b" or api_key == "sk-prod-*"'
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
            <p className="text-xs text-muted-foreground mt-1.5">
              Use <code className="text-primary font-medium">=~</code> for regex match, <code className="text-primary font-medium">==</code> for exact match
            </p>
          </div>
          
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Routing Strategy
            </label>
            <div className="space-y-2">
              {STRATEGIES.map((strategy) => (
                <label
                  key={strategy.value}
                  className={`flex items-center gap-3 p-3 rounded-lg border cursor-pointer transition-colors ${
                    newRuleForm.strategy === strategy.value
                      ? 'border-primary/50 bg-primary/5'
                      : 'border-border bg-secondary hover:border-border/80'
                  }`}
                >
                  <input
                    type="radio"
                    name="strategy"
                    value={strategy.value}
                    checked={newRuleForm.strategy === strategy.value}
                    onChange={(e) => setNewRuleForm({ ...newRuleForm, strategy: e.target.value as any })}
                    className="accent-primary"
                  />
                  <div>
                    <p className="text-sm font-medium text-foreground">{strategy.label}</p>
                    <p className="text-xs text-muted-foreground">{strategy.description}</p>
                  </div>
                </label>
              ))}
            </div>
          </div>
          
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsCreateModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleCreateRule}
              className="px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Add Rule
            </button>
          </div>
        </div>
      </Modal>

      {/* Delete Confirmation Modal */}
      <Modal
        isOpen={isDeleteModalOpen}
        onClose={() => {
          setIsDeleteModalOpen(false);
          setRuleToDelete(null);
        }}
        title="Delete Routing Rule"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to delete the routing rule with priority{' '}
            <span className="text-foreground font-medium">#{ruleToDelete?.priority}</span>?
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsDeleteModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleDeleteRule}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Delete Rule
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
