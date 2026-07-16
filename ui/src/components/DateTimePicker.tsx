import { useState, useRef, useEffect } from 'react';
import { Calendar as CalendarIcon, Clock, ChevronLeft, ChevronRight, ChevronUp, ChevronDown, X } from 'lucide-react';

interface CustomDateTimePickerProps {
  value: string; // YYYY-MM-DDTHH:MM
  onChange: (value: string) => void;
  placeholder?: string;
  className?: string;
  disabled?: boolean;
}

export function CustomDateTimePicker({
  value,
  onChange,
  placeholder = 'Select date & time...',
  className = '',
  disabled = false,
}: CustomDateTimePickerProps) {
  const [isOpen, setIsOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  // Parsing state
  const initialDate = value ? new Date(value) : new Date();
  const [viewYear, setViewYear] = useState(initialDate.getFullYear());
  const [viewMonth, setViewMonth] = useState(initialDate.getMonth()); // 0-11
  
  // Hour & Minute values
  const [hours, setHours] = useState(value ? new Date(value).getHours() : 0);
  const [minutes, setMinutes] = useState(value ? new Date(value).getMinutes() : 0);
  const [selectedDay, setSelectedDay] = useState<number | null>(
    value ? new Date(value).getDate() : null
  );

  // Sync state with incoming value changes
  useEffect(() => {
    if (value) {
      const d = new Date(value);
      setViewYear(d.getFullYear());
      setViewMonth(d.getMonth());
      setSelectedDay(d.getDate());
      setHours(d.getHours());
      setMinutes(d.getMinutes());
    } else {
      setSelectedDay(null);
    }
  }, [value]);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const months = [
    'January', 'February', 'March', 'April', 'May', 'June',
    'July', 'August', 'September', 'October', 'November', 'December'
  ];

  const daysOfWeek = ['Su', 'Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa'];

  // Helper calendar calculations
  const getDaysInMonth = (y: number, m: number) => new Date(y, m + 1, 0).getDate();
  const getFirstDayOfMonth = (y: number, m: number) => new Date(y, m, 1).getDay();

  const handlePrevMonth = () => {
    if (viewMonth === 0) {
      setViewMonth(11);
      setViewYear(viewYear - 1);
    } else {
      setViewMonth(viewMonth - 1);
    }
  };

  const handleNextMonth = () => {
    if (viewMonth === 11) {
      setViewMonth(0);
      setViewYear(viewYear + 1);
    } else {
      setViewMonth(viewMonth + 1);
    }
  };

  const selectDay = (day: number) => {
    setSelectedDay(day);
    updateValue(day, hours, minutes);
  };

  const handleHourChange = (h: number) => {
    // Wrap hours around 0-23
    let val = h;
    if (h > 23) val = 0;
    if (h < 0) val = 23;
    setHours(val);
    if (selectedDay !== null) {
      updateValue(selectedDay, val, minutes);
    }
  };

  const handleMinuteChange = (m: number) => {
    // Wrap minutes around 0-59
    let val = m;
    if (m > 59) val = 0;
    if (m < 0) val = 59;
    setMinutes(val);
    if (selectedDay !== null) {
      updateValue(selectedDay, hours, val);
    }
  };

  const updateValue = (day: number, h: number, m: number) => {
    const date = new Date(viewYear, viewMonth, day, h, m);
    const yyyy = date.getFullYear();
    const mm = String(date.getMonth() + 1).padStart(2, '0');
    const dd = String(date.getDate()).padStart(2, '0');
    const hh = String(date.getHours()).padStart(2, '0');
    const min = String(date.getMinutes()).padStart(2, '0');
    onChange(`${yyyy}-${mm}-${dd}T${hh}:${min}`);
  };

  const handleClear = () => {
    onChange('');
    setSelectedDay(null);
    setIsOpen(false);
  };

  // Generate calendar days grid
  const daysInMonth = getDaysInMonth(viewYear, viewMonth);
  const firstDayIndex = getFirstDayOfMonth(viewYear, viewMonth);

  const prevMonthDaysCount = getDaysInMonth(
    viewMonth === 0 ? viewYear - 1 : viewYear,
    viewMonth === 0 ? 11 : viewMonth - 1
  );

  const calendarGrid = [];

  // Previous month padding days
  for (let i = firstDayIndex - 1; i >= 0; i--) {
    calendarGrid.push({
      day: prevMonthDaysCount - i,
      isCurrentMonth: false,
      monthOffset: -1
    });
  }

  // Current month days
  for (let i = 1; i <= daysInMonth; i++) {
    calendarGrid.push({
      day: i,
      isCurrentMonth: true,
      monthOffset: 0
    });
  }

  // Next month padding days
  const remainingCells = 42 - calendarGrid.length;
  for (let i = 1; i <= remainingCells; i++) {
    calendarGrid.push({
      day: i,
      isCurrentMonth: false,
      monthOffset: 1
    });
  }

  // Format display text
  const formatDisplay = () => {
    if (!value) return '';
    try {
      const d = new Date(value);
      if (isNaN(d.getTime())) return value;
      return d.toLocaleString(undefined, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
      });
    } catch {
      return value;
    }
  };

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <div className="relative">
        <button
          type="button"
          disabled={disabled}
          onClick={() => setIsOpen(!isOpen)}
          className={`flex items-center justify-between w-full border border-border bg-secondary/30 text-foreground px-3 py-2 text-sm rounded-lg text-left transition-all ${
            disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer hover:bg-secondary/50'
          }`}
        >
          <span className={`truncate ${!value ? 'text-muted-foreground/60' : ''}`}>
            {value ? formatDisplay() : placeholder}
          </span>
          <CalendarIcon className="w-4 h-4 ml-2 shrink-0 text-primary" />
        </button>

        {!!value && !disabled && (
          <button
            type="button"
            onClick={handleClear}
            className="absolute right-8 top-1/2 -translate-y-1/2 p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {isOpen && (
        <div className="absolute z-50 mt-1.5 p-4 border border-border bg-card rounded-xl shadow-xl w-80 animate-fade-in">
          {/* Calendar header */}
          <div className="flex items-center justify-between mb-3">
            <span className="font-semibold text-sm text-foreground">
              {months[viewMonth]} {viewYear}
            </span>
            <div className="flex items-center gap-1">
              <button
                type="button"
                onClick={handlePrevMonth}
                className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
              >
                <ChevronLeft className="w-4 h-4" />
              </button>
              <button
                type="button"
                onClick={handleNextMonth}
                className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
              >
                <ChevronRight className="w-4 h-4" />
              </button>
            </div>
          </div>

          {/* Days of week header */}
          <div className="grid grid-cols-7 gap-1 text-center text-xs font-semibold text-muted-foreground/75 mb-2">
            {daysOfWeek.map((d) => (
              <div key={d} className="py-0.5">{d}</div>
            ))}
          </div>

          {/* Calendar days grid */}
          <div className="grid grid-cols-7 gap-1 mb-4">
            {calendarGrid.map((cell, idx) => {
              const isSelected =
                cell.isCurrentMonth && selectedDay === cell.day;
              
              const isToday =
                cell.isCurrentMonth &&
                new Date().getDate() === cell.day &&
                new Date().getMonth() === viewMonth &&
                new Date().getFullYear() === viewYear;

              return (
                <button
                  key={idx}
                  type="button"
                  onClick={() => {
                    if (cell.isCurrentMonth) {
                      selectDay(cell.day);
                    } else {
                      // Switch month
                      const targetDate = new Date(viewYear, viewMonth + cell.monthOffset, cell.day);
                      setViewYear(targetDate.getFullYear());
                      setViewMonth(targetDate.getMonth());
                      setSelectedDay(targetDate.getDate());
                      updateValue(targetDate.getDate(), hours, minutes);
                    }
                  }}
                  className={`text-xs py-1.5 rounded-lg text-center font-medium transition-colors ${
                    isSelected
                      ? 'bg-primary text-primary-foreground font-semibold shadow-sm'
                      : cell.isCurrentMonth
                      ? 'text-foreground hover:bg-secondary/60'
                      : 'text-muted-foreground/40 hover:bg-secondary/40'
                  } ${isToday && !isSelected ? 'border border-primary/50' : ''}`}
                >
                  {cell.day}
                </button>
              );
            })}
          </div>

          {/* Premium Time Picker Row */}
          <div className="border-t border-border/80 pt-3 flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Clock className="w-3.5 h-3.5 text-primary shrink-0" />
              <span className="font-semibold">Time (24h)</span>
            </div>

            <div className="flex items-center gap-1.5 font-mono">
              {/* Hour control */}
              <div className="flex flex-col items-center select-none">
                <button
                  type="button"
                  onClick={() => handleHourChange(hours + 1)}
                  className="p-0.5 rounded text-muted-foreground hover:text-primary hover:bg-secondary/50 transition-colors"
                >
                  <ChevronUp className="w-3.5 h-3.5" />
                </button>
                <div className="w-10 py-1 text-center bg-secondary/50 border border-border rounded-lg text-foreground text-sm font-semibold">
                  {String(hours).padStart(2, '0')}
                </div>
                <button
                  type="button"
                  onClick={() => handleHourChange(hours - 1)}
                  className="p-0.5 rounded text-muted-foreground hover:text-primary hover:bg-secondary/50 transition-colors"
                >
                  <ChevronDown className="w-3.5 h-3.5" />
                </button>
              </div>

              <span className="text-muted-foreground font-bold text-lg -mt-3">:</span>

              {/* Minute control */}
              <div className="flex flex-col items-center select-none">
                <button
                  type="button"
                  onClick={() => handleMinuteChange(minutes + 1)}
                  className="p-0.5 rounded text-muted-foreground hover:text-primary hover:bg-secondary/50 transition-colors"
                >
                  <ChevronUp className="w-3.5 h-3.5" />
                </button>
                <div className="w-10 py-1 text-center bg-secondary/50 border border-border rounded-lg text-foreground text-sm font-semibold">
                  {String(minutes).padStart(2, '0')}
                </div>
                <button
                  type="button"
                  onClick={() => handleMinuteChange(minutes - 1)}
                  className="p-0.5 rounded text-muted-foreground hover:text-primary hover:bg-secondary/50 transition-colors"
                >
                  <ChevronDown className="w-3.5 h-3.5" />
                </button>
              </div>
            </div>
          </div>

          {/* Action buttons */}
          <div className="flex justify-end gap-2 mt-4 pt-3 border-t border-border/80 text-xs">
            <button
              type="button"
              onClick={handleClear}
              className="px-2.5 py-1.5 text-muted-foreground hover:text-foreground transition-colors"
            >
              Clear
            </button>
            <button
              type="button"
              onClick={() => setIsOpen(false)}
              className="px-3 py-1.5 bg-primary text-primary-foreground font-medium rounded-lg hover:bg-primary/95 transition-colors"
            >
              Done
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

interface CustomTimePickerProps {
  value: string; // HH:MM
  onChange: (value: string) => void;
  placeholder?: string;
  className?: string;
  disabled?: boolean;
}

export function CustomTimePicker({
  value,
  onChange,
  placeholder = 'Select time...',
  className = '',
  disabled = false,
}: CustomTimePickerProps) {
  const [isOpen, setIsOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  const [hours, setHours] = useState(value ? parseInt(value.split(':')[0], 10) : 0);
  const [minutes, setMinutes] = useState(value ? parseInt(value.split(':')[1], 10) : 0);

  useEffect(() => {
    if (value && value.includes(':')) {
      const parts = value.split(':');
      setHours(parseInt(parts[0], 10));
      setMinutes(parseInt(parts[1], 10));
    }
  }, [value]);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const handleHourChange = (h: number) => {
    let val = h;
    if (h > 23) val = 0;
    if (h < 0) val = 23;
    setHours(val);
    updateValue(val, minutes);
  };

  const handleMinuteChange = (m: number) => {
    let val = m;
    if (m > 59) val = 0;
    if (m < 0) val = 59;
    setMinutes(val);
    updateValue(hours, val);
  };

  const updateValue = (h: number, m: number) => {
    const hh = String(h).padStart(2, '0');
    const mm = String(m).padStart(2, '0');
    onChange(`${hh}:${mm}`);
  };

  const hourOptions = Array.from({ length: 24 }, (_, i) => i);
  const minuteOptions = Array.from({ length: 60 }, (_, i) => i);

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <div className="relative">
        <button
          type="button"
          disabled={disabled}
          onClick={() => setIsOpen(!isOpen)}
          className={`flex items-center justify-between w-full border border-border bg-secondary/30 text-foreground px-3 py-2 text-sm rounded-lg text-left transition-all ${
            disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer hover:bg-secondary/50'
          }`}
        >
          <span className={`truncate font-mono ${!value ? 'text-muted-foreground/60' : ''}`}>
            {value ? value : placeholder}
          </span>
          <Clock className="w-4 h-4 ml-2 shrink-0 text-primary" />
        </button>

        {!!value && !disabled && (
          <button
            type="button"
            onClick={() => onChange('')}
            className="absolute right-8 top-1/2 -translate-y-1/2 p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {isOpen && (
        <div className="absolute z-50 mt-1.5 p-4 border border-border bg-card rounded-xl shadow-xl w-64 animate-fade-in">
          {/* Main Large Clock Display */}
          <div className="flex justify-center items-center gap-1.5 font-mono text-3xl font-bold text-primary mb-5 select-none bg-secondary/20 py-2.5 rounded-xl border border-border/40">
            <span>{String(hours).padStart(2, '0')}</span>
            <span className="text-muted-foreground/50 animate-pulse">:</span>
            <span>{String(minutes).padStart(2, '0')}</span>
          </div>

          {/* Premium Drum Wheel Picker */}
          <div className="flex gap-4 mb-4 relative h-40 border border-border/80 rounded-xl bg-secondary/10 p-1.5 overflow-hidden">
            {/* Soft linear fade mask overlays */}
            <div className="absolute top-0 left-0 right-0 h-10 bg-gradient-to-b from-card to-transparent pointer-events-none z-10" />
            <div className="absolute bottom-0 left-0 right-0 h-10 bg-gradient-to-t from-card to-transparent pointer-events-none z-10" />
            
            {/* Center target indicator overlay */}
            <div className="absolute top-1/2 -translate-y-1/2 left-2 right-2 h-9 border border-primary/20 bg-primary/5 rounded-lg pointer-events-none z-0" />

            {/* Hours Column */}
            <ScrollWheel
              options={hourOptions}
              value={hours}
              onChange={handleHourChange}
              label="Hours"
            />

            {/* Minutes Column */}
            <ScrollWheel
              options={minuteOptions}
              value={minutes}
              onChange={handleMinuteChange}
              label="Minutes"
            />
          </div>

          {/* Footer Action buttons */}
          <div className="flex justify-end gap-2 mt-4 pt-3 border-t border-border/80 text-xs">
            <button
              type="button"
              onClick={() => {
                onChange('');
                setIsOpen(false);
              }}
              className="px-2.5 py-1.5 text-muted-foreground hover:text-foreground transition-colors"
            >
              Clear
            </button>
            <button
              type="button"
              onClick={() => setIsOpen(false)}
              className="px-3 py-1.5 bg-primary text-primary-foreground font-medium rounded-lg hover:bg-primary/95 transition-colors"
            >
              Done
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

interface ScrollWheelProps {
  options: number[];
  value: number;
  onChange: (val: number) => void;
  label: string;
}

function ScrollWheel({ options, value, onChange, label }: ScrollWheelProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const isScrollingRef = useRef<boolean>(false);
  const scrollTimeoutRef = useRef<any>(null);

  // Initial scroll-into-view on load
  useEffect(() => {
    const activeEl = containerRef.current?.querySelector(`[data-value="${value}"]`);
    if (activeEl) {
      activeEl.scrollIntoView({ block: 'center' });
    }
  }, []);

  // Sync scroll when value changes externally (or via click)
  useEffect(() => {
    if (!isScrollingRef.current) {
      const activeEl = containerRef.current?.querySelector(`[data-value="${value}"]`);
      if (activeEl) {
        activeEl.scrollIntoView({ behavior: 'smooth', block: 'center' });
      }
    }
  }, [value]);

  const handleScroll = (e: React.UIEvent<HTMLDivElement>) => {
    isScrollingRef.current = true;

    if (scrollTimeoutRef.current) {
      clearTimeout(scrollTimeoutRef.current);
    }

    // Scroll snapping detection (Height of each item is 28px)
    scrollTimeoutRef.current = setTimeout(() => {
      if (containerRef.current) {
        const scrollTop = containerRef.current.scrollTop;
        const index = Math.round(scrollTop / 28);
        if (options[index] !== undefined && options[index] !== value) {
          onChange(options[index]);
        }
        isScrollingRef.current = false;
      }
    }, 150);
  };

  return (
    <div className="flex-1 flex flex-col h-full relative">
      <div
        ref={containerRef}
        onScroll={handleScroll}
        className="h-full overflow-y-auto no-scrollbar py-[66px] snap-y snap-mandatory scroll-smooth z-10 relative"
      >
        {options.map((opt) => {
          const isSelected = opt === value;
          return (
            <button
              key={opt}
              type="button"
              data-value={opt}
              onClick={() => {
                onChange(opt);
              }}
              className={`block w-full py-1 text-center font-mono text-xs snap-center transition-all ${
                isSelected
                  ? 'text-primary font-bold text-sm scale-110'
                  : 'text-foreground/40 hover:text-foreground/80 scale-95'
              }`}
            >
              {String(opt).padStart(2, '0')}
            </button>
          );
        })}
      </div>
    </div>
  );
}
