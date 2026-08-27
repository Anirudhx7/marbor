import { useState, useRef, useEffect } from 'react';
import { createPortal } from 'react-dom';
import { ChevronDown, Check, X } from 'lucide-react';

// Dropdown menus render inside a scrollable modal body (Modal.tsx uses
// overflow-y-auto). An `absolute`-positioned menu gets clipped at that
// container's edge regardless of top/bottom flip math, which is what
// produced the "cut off at bottom" bug. Portaling to document.body with
// `position: fixed` coordinates (recomputed from the trigger's
// getBoundingClientRect, same pattern Modal.tsx already uses) escapes any
// ancestor's overflow clipping.
interface MenuRect {
  left: number;
  width: number;
  // Viewport-relative distance from the trigger's top edge to the top of
  // the viewport, and from the trigger's bottom edge to the bottom of the
  // viewport - the two numbers `position: fixed; top:`/`bottom:` need
  // directly, so the render side has no follow-up math to get wrong.
  triggerTopFromViewportTop: number;
  triggerBottomFromViewportBottom: number;
  openUp: boolean;
}

function useMenuPosition(containerRef: React.RefObject<HTMLDivElement | null>, isOpen: boolean) {
  const [rect, setRect] = useState<MenuRect | null>(null);

  useEffect(() => {
    if (!isOpen) return;
    const update = () => {
      if (!containerRef.current) return;
      const r = containerRef.current.getBoundingClientRect();
      const spaceBelow = window.innerHeight - r.bottom;
      const openUp = spaceBelow < 260 && r.top > spaceBelow;
      // Clamp to viewport on 375px devices - trigger may be 343px wide at
      // left 16px, but inside a modal with p-4 the right edge at 359px is
      // within 8px margin (367 max). Clamping guarantees no overflow even
      // if trigger sits in a non-first grid column or near the right edge.
      const VIEWPORT_MARGIN = 8;
      const vw = window.innerWidth;
      const clampedWidth = Math.min(r.width, vw - VIEWPORT_MARGIN * 2);
      const clampedLeft = Math.max(VIEWPORT_MARGIN, Math.min(r.left, vw - clampedWidth - VIEWPORT_MARGIN));
      setRect({
        left: clampedLeft,
        width: clampedWidth,
        triggerTopFromViewportTop: r.top,
        triggerBottomFromViewportBottom: window.innerHeight - r.bottom,
        openUp,
      });
    };
    update();
    window.addEventListener('scroll', update, true);
    window.addEventListener('resize', update);
    // Mobile keyboard changes visualViewport height without firing window resize on some browsers
    window.visualViewport?.addEventListener('resize', update);
    window.visualViewport?.addEventListener('scroll', update);
    return () => {
      window.removeEventListener('scroll', update, true);
      window.removeEventListener('resize', update);
      window.visualViewport?.removeEventListener('resize', update);
      window.visualViewport?.removeEventListener('scroll', update);
    };
  }, [isOpen, containerRef]);

  return rect;
}

// Shared gap between the trigger and the menu, and the hard floor/ceiling on
// menu height - big enough to be usable, small enough to always fit in the
// remaining space on that side of the trigger (this is what makes behavior
// consistent regardless of where the trigger sits in the viewport, instead
// of the old fixed `max-h-60` which could exceed the actual room available
// and get clipped).
const MENU_GAP = 6;
const MENU_MIN_HEIGHT = 120;
const MENU_MAX_HEIGHT = 288;

function menuFixedStyle(rect: MenuRect): React.CSSProperties {
  const base: React.CSSProperties = {
    position: 'fixed',
    left: rect.left,
    width: rect.width,
  };
  if (rect.openUp) {
    const available = rect.triggerTopFromViewportTop - MENU_GAP * 2;
    return {
      ...base,
      bottom: window.innerHeight - rect.triggerTopFromViewportTop + MENU_GAP,
      maxHeight: Math.min(Math.max(available, MENU_MIN_HEIGHT), MENU_MAX_HEIGHT),
    };
  }
  const available = rect.triggerBottomFromViewportBottom - MENU_GAP * 2;
  return {
    ...base,
    top: window.innerHeight - rect.triggerBottomFromViewportBottom + MENU_GAP,
    maxHeight: Math.min(Math.max(available, MENU_MIN_HEIGHT), MENU_MAX_HEIGHT),
  };
}

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
  const menuRef = useRef<HTMLDivElement>(null);
  const menuRect = useMenuPosition(containerRef, isOpen);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent | TouchEvent) {
      const target = event.target as Node;
      if (
        containerRef.current && !containerRef.current.contains(target) &&
        menuRef.current && !menuRef.current.contains(target)
      ) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    document.addEventListener('touchstart', handleClickOutside, { passive: true } as AddEventListenerOptions);
    return () => {
      document.removeEventListener('mousedown', handleClickOutside);
      document.removeEventListener('touchstart', handleClickOutside);
    };
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

      {isOpen && menuRect && createPortal(
        <div
          ref={menuRef}
          style={{ ...menuFixedStyle(menuRect), zIndex: 9999 }}
          className="border border-border bg-card rounded-lg shadow-xl overflow-y-auto animate-fade-in focus:outline-none"
        >
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
                    // Touch devices need touchEnd to avoid 300ms delay / ghost mousedown closing before click fires
                    onTouchEnd={(e) => {
                      e.preventDefault();
                      onChange(o.value);
                      setIsOpen(false);
                    }}
                    className={`flex items-center justify-between w-full px-3 py-2 text-left text-sm transition-colors hover:bg-primary/10 hover:text-primary min-h-[40px] sm:min-h-0 ${
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
        </div>,
        document.body
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
  const menuRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const menuRect = useMenuPosition(containerRef, isOpen);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent | TouchEvent) {
      const target = event.target as Node;
      if (
        containerRef.current && !containerRef.current.contains(target) &&
        menuRef.current && !menuRef.current.contains(target)
      ) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    document.addEventListener('touchstart', handleClickOutside, { passive: true } as AddEventListenerOptions);
    return () => {
      document.removeEventListener('mousedown', handleClickOutside);
      document.removeEventListener('touchstart', handleClickOutside);
    };
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

      {isOpen && menuRect && createPortal(
        <div
          ref={menuRef}
          style={{ ...menuFixedStyle(menuRect), zIndex: 9999 }}
          className="border border-border bg-card rounded-lg shadow-xl overflow-y-auto animate-fade-in focus:outline-none"
        >
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
                    onTouchEnd={(e) => {
                      e.preventDefault();
                      onChange(opt);
                      setIsOpen(false);
                    }}
                    className={`flex items-center justify-between w-full px-3 py-2 text-left text-sm transition-colors hover:bg-primary/10 hover:text-primary min-h-[40px] sm:min-h-0 ${
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
        </div>,
        document.body
      )}
    </div>
  );
}

interface CustomTagComboboxProps {
  // Raw comma-separated text, exactly as typed/built - callers already parse
  // this with `.split(',').map(s => s.trim()).filter(Boolean)`.
  value: string;
  onChange: (value: string) => void;
  options: string[];
  placeholder?: string;
  className?: string;
  disabled?: boolean;
}

// CustomTagCombobox is CustomCombobox's sibling for a comma-separated list
// value (e.g. an ordered chain of fallback model names) instead of a single
// value. Autocompletes against the segment currently being typed (after the
// last comma) and, on selection, appends the choice as a completed segment
// rather than replacing the whole field - so multiple picks compose instead
// of overwriting each other. Arbitrary typed text always remains valid,
// same convention as CustomCombobox.
export function CustomTagCombobox({
  value,
  onChange,
  options,
  placeholder = '',
  className = '',
  disabled = false,
}: CustomTagComboboxProps) {
  const [isOpen, setIsOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const menuRect = useMenuPosition(containerRef, isOpen);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent | TouchEvent) {
      const target = event.target as Node;
      if (
        containerRef.current && !containerRef.current.contains(target) &&
        menuRef.current && !menuRef.current.contains(target)
      ) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    document.addEventListener('touchstart', handleClickOutside, { passive: true } as AddEventListenerOptions);
    return () => {
      document.removeEventListener('mousedown', handleClickOutside);
      document.removeEventListener('touchstart', handleClickOutside);
    };
  }, []);

  const segments = value.split(',');
  const completedNames = segments.slice(0, -1).map((s) => s.trim()).filter(Boolean);
  const currentSegment = segments[segments.length - 1].trim();

  const filteredOptions = options.filter(
    (opt) => !completedNames.includes(opt) && opt.toLowerCase().includes(currentSegment.toLowerCase())
  );

  const selectOption = (opt: string) => {
    onChange([...completedNames, opt].join(', ') + ', ');
    inputRef.current?.focus();
  };

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <div className="relative">
        <input
          ref={inputRef}
          type="text"
          disabled={disabled}
          placeholder={placeholder}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          onFocus={() => setIsOpen(true)}
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

      {isOpen && menuRect && createPortal(
        <div
          ref={menuRef}
          style={{ ...menuFixedStyle(menuRect), zIndex: 9999 }}
          className="border border-border bg-card rounded-lg shadow-xl overflow-y-auto animate-fade-in focus:outline-none"
        >
          <div className="py-1">
            {filteredOptions.length === 0 ? (
              <div className="px-3 py-2 text-sm text-muted-foreground text-center">
                {options.length === 0 ? 'No options found' : 'All matching models already added'}
              </div>
            ) : (
              filteredOptions.map((opt) => (
                <button
                  key={opt}
                  type="button"
                  onClick={() => selectOption(opt)}
                  onTouchEnd={(e) => {
                    e.preventDefault();
                    selectOption(opt);
                  }}
                  className="flex items-center w-full px-3 py-2 text-left text-sm text-foreground transition-colors hover:bg-primary/10 hover:text-primary hover:bg-secondary/40 min-h-[40px] sm:min-h-0"
                >
                  <span className="truncate">{opt}</span>
                </button>
              ))
            )}
          </div>
        </div>,
        document.body
      )}
    </div>
  );
}
