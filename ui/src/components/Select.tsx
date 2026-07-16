import { useState, useRef, useEffect } from 'react';
import { ChevronDown, Check, X } from 'lucide-react';

export interface SelectOption {
  value: string;
  label: string;
}

interface CustomSelectProps {
  value: string;
  onChange: (value: string) => void;
  options: SelectOption[];
  className?: string;
  disabled?: boolean;
  placeholder?: string;
  size?: 'sm' | 'md';
}

export function CustomSelect({
  value,
  onChange,
  options,
  className = '',
  disabled = false,
  placeholder = 'Select...',
  size = 'md',
}: CustomSelectProps) {
  const [isOpen, setIsOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  const [position, setPosition] = useState<'bottom' | 'top'>('bottom');

  useEffect(() => {
    if (!isOpen) return;
    const updatePosition = () => {
      if (!containerRef.current) return;
      const rect = containerRef.current.getBoundingClientRect();
      const spaceBelow = window.innerHeight - rect.bottom;
      setPosition(spaceBelow < 260 ? 'top' : 'bottom');
    };
    updatePosition();
    window.addEventListener('scroll', updatePosition, true);
    window.addEventListener('resize', updatePosition);
    return () => {
      window.removeEventListener('scroll', updatePosition, true);
      window.removeEventListener('resize', updatePosition);
    };
  }, [isOpen]);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const selectedOption = options.find((o) => o.value === value);

  const sizeClasses = size === 'sm' ? 'px-2.5 py-1 text-xs rounded-md' : 'px-3 py-2 text-sm rounded-lg';

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <button
        type="button"
        disabled={disabled}
        onClick={() => setIsOpen(!isOpen)}
        className={`flex items-center justify-between w-full border border-border bg-secondary/30 text-foreground focus:outline-none focus:ring-2 focus:ring-primary/40 text-left transition-all ${sizeClasses} ${
          disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer hover:bg-secondary/50'
        }`}
      >
        <span className="truncate">{selectedOption ? selectedOption.label : placeholder}</span>
        <ChevronDown className={`w-4 h-4 ml-2 shrink-0 text-muted-foreground/80 transition-transform duration-200 ${isOpen ? 'rotate-180' : ''}`} />
      </button>

      {isOpen && (
        <div className={`absolute z-50 w-full border border-border bg-card rounded-lg shadow-xl max-h-60 overflow-y-auto animate-fade-in focus:outline-none ${
          position === 'top' ? 'bottom-full mb-1.5' : 'top-full mt-1.5'
        }`}>
          <div className="py-1">
            {options.length === 0 ? (
              <div className="px-3 py-2 text-sm text-muted-foreground text-center">No options</div>
            ) : (
              options.map((o) => {
                const isSelected = o.value === value;
                return (
                  <button
                    key={o.value}
                    type="button"
                    onClick={() => {
                      onChange(o.value);
                      setIsOpen(false);
                    }}
                    className={`flex items-center justify-between w-full px-3 py-2 text-left text-sm transition-colors hover:bg-primary/10 hover:text-primary ${
                      isSelected ? 'bg-primary/5 text-primary font-medium' : 'text-foreground hover:bg-secondary/40'
                    }`}
                  >
                    <span className="truncate">{o.label}</span>
                    {isSelected && <Check className="w-4 h-4 ml-2 text-primary shrink-0" />}
                  </button>
                );
              })
            )}
          </div>
        </div>
      )}
    </div>
  );
}

interface CustomComboboxProps {
  value: string;
  onChange: (value: string) => void;
  options: string[];
  placeholder?: string;
  className?: string;
  disabled?: boolean;
}

export function CustomCombobox({
  value,
  onChange,
  options,
  placeholder = '',
  className = '',
  disabled = false,
}: CustomComboboxProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [searchTerm, setSearchTerm] = useState('');
  const containerRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const [position, setPosition] = useState<'bottom' | 'top'>('bottom');

  useEffect(() => {
    if (!isOpen) return;
    const updatePosition = () => {
      if (!containerRef.current) return;
      const rect = containerRef.current.getBoundingClientRect();
      const spaceBelow = window.innerHeight - rect.bottom;
      setPosition(spaceBelow < 260 ? 'top' : 'bottom');
    };
    updatePosition();
    window.addEventListener('scroll', updatePosition, true);
    window.addEventListener('resize', updatePosition);
    return () => {
      window.removeEventListener('scroll', updatePosition, true);
      window.removeEventListener('resize', updatePosition);
    };
  }, [isOpen]);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const filteredOptions = options.filter((opt) =>
    opt.toLowerCase().includes(searchTerm.toLowerCase())
  );

  const displayOptions = searchTerm ? filteredOptions : options;

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <div className="relative">
        <input
          ref={inputRef}
          type="text"
          disabled={disabled}
          placeholder={placeholder}
          value={isOpen ? searchTerm : value}
          onChange={(e) => {
            setSearchTerm(e.target.value);
            onChange(e.target.value);
            if (!isOpen) setIsOpen(true);
          }}
          onFocus={() => {
            setSearchTerm('');
            setIsOpen(true);
          }}
          className={`w-full pr-8 px-3 py-2 text-sm rounded-lg border border-border bg-secondary/30 text-foreground placeholder:text-muted-foreground/60 focus:outline-none focus:ring-2 focus:ring-primary/40 transition-all ${
            disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-text hover:bg-secondary/40'
          }`}
        />
        <div className="absolute right-1.5 top-1/2 -translate-y-1/2 flex items-center gap-0.5 animate-fade-in">
          {!!value && (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onChange('');
                setSearchTerm('');
                setIsOpen(false);
              }}
              className="p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
            >
              <X className="w-3.5 h-3.5" />
            </button>
          )}
          <button
            type="button"
            disabled={disabled}
            onClick={() => {
              if (isOpen) {
                setIsOpen(false);
              } else {
                setSearchTerm('');
                setIsOpen(true);
                inputRef.current?.focus();
              }
            }}
            className="p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
          >
            <ChevronDown className={`w-4 h-4 transition-transform duration-200 ${isOpen ? 'rotate-180' : ''}`} />
          </button>
        </div>
      </div>

      {isOpen && (
        <div className={`absolute z-50 w-full border border-border bg-card rounded-lg shadow-xl max-h-60 overflow-y-auto animate-fade-in focus:outline-none ${
          position === 'top' ? 'bottom-full mb-1.5' : 'top-full mt-1.5'
        }`}>
          <div className="py-1">
            {displayOptions.length === 0 ? (
              <div className="px-3 py-2 text-sm text-muted-foreground text-center">No options found</div>
            ) : (
              displayOptions.map((opt) => {
                const isSelected = opt === value;
                return (
                  <button
                    key={opt}
                    type="button"
                    onClick={() => {
                      onChange(opt);
                      setIsOpen(false);
                    }}
                    className={`flex items-center justify-between w-full px-3 py-2 text-left text-sm transition-colors hover:bg-primary/10 hover:text-primary ${
                      isSelected ? 'bg-primary/5 text-primary font-medium' : 'text-foreground hover:bg-secondary/40'
                    }`}
                  >
                    <span className="truncate">{opt}</span>
                    {isSelected && <Check className="w-4 h-4 ml-2 text-primary shrink-0" />}
                  </button>
                );
              })
            )}
          </div>
        </div>
      )}
    </div>
  );
}
