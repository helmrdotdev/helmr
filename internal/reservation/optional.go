package reservation

// AdoptOptional transfers an existing physical charge to a real allocation.
// Excess reservation remains charged until its owner proves physical release.
// There is no intermediate free slot and a source can be adopted only once.
func (l *Ledger) AdoptOptional(from, to Key, required Vector) error {
	if err := validateKey(from); err != nil {
		return err
	}
	if err := validateKey(to); err != nil {
		return err
	}
	if err := validateRequest(required); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	reserved, ok := l.optional[from]
	if !ok {
		return ErrInvalidKey
	}
	if _, exists := l.reservations[to]; exists {
		return ErrDuplicateReservation
	}
	if !fits(required, reserved) {
		return ErrCapacityExceeded
	}
	delete(l.optional, from)
	delete(l.reservations, from)
	l.reservations[to] = reserved
	close(l.changed)
	l.changed = make(chan struct{})
	return nil
}

// ReserveOptional applies the combined optional ceiling and unborrowable local
// headroom atomically with ordinary reservations. Policy owners serialize policy
// changes with this call. Lowering a ceiling does not release existing physical
// resources: Release is still required after their owners finish cleanup.
func (l *Ledger) ReserveOptional(key Key, request, ceiling, headroom Vector) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	if err := validateRequest(request); err != nil {
		return false, err
	}
	if !nonnegative(ceiling) || !nonnegative(headroom) {
		return false, ErrInvalidRequest
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if reserved, ok := l.reservations[key]; ok {
		if optional, exists := l.optional[key]; exists && reserved == request && optional == request {
			return false, nil
		}
		return false, ErrDuplicateReservation
	}
	optional := request
	for _, reserved := range l.optional {
		var err error
		optional, err = add(optional, reserved)
		if err != nil {
			return false, err
		}
	}
	if !fits(optional, ceiling) {
		return false, ErrCapacityExceeded
	}
	next, err := add(l.used, request)
	if err != nil {
		return false, err
	}
	protected, err := add(next, headroom)
	if err != nil {
		return false, err
	}
	if !fits(protected, l.capacity) {
		return false, ErrCapacityExceeded
	}
	l.used = next
	l.reservations[key] = request
	l.optional[key] = request
	return true, nil
}

func nonnegative(v Vector) bool {
	return v.CPUMillis >= 0 && v.MemoryBytes >= 0 && v.HostDiskBytes >= 0 && v.VMSlots >= 0
}

// ResizeOptional changes an existing physical charge. The caller must prove
// storage/process release before decreasing it; growth is checked atomically
// against ordinary reservations, optional ceilings and protected headroom.
func (l *Ledger) ResizeOptional(key Key, request, ceiling, headroom Vector) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	if !nonnegative(ceiling) || !nonnegative(headroom) {
		return ErrInvalidRequest
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	old, exists := l.optional[key]
	if !exists {
		return ErrInvalidKey
	}
	next, err := add(subtract(l.used, old), request)
	if err != nil {
		return err
	}
	if !fits(request, old) {
		optional := request
		for other, reserved := range l.optional {
			if other == key {
				continue
			}
			optional, err = add(optional, reserved)
			if err != nil {
				return err
			}
		}
		protected, err := add(next, headroom)
		if err != nil {
			return err
		}
		if !fits(optional, ceiling) || !fits(protected, l.capacity) {
			return ErrCapacityExceeded
		}
	}
	l.used = next
	l.reservations[key], l.optional[key] = request, request
	close(l.changed)
	l.changed = make(chan struct{})
	return nil
}
