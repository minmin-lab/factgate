SELECT receipt_no, employee_no, employee_name, department, expense_date, expense_type, amount, city, purpose, status
FROM (
    SELECT e.*,
           SUM(e.amount) OVER (PARTITION BY e.expense_type) AS type_total,
           COUNT(*) OVER (PARTITION BY e.expense_type) AS type_count
    FROM expense_detail e
) t
WHERE t.amount * t.type_count > 2 * t.type_total
ORDER BY expense_type, amount DESC, receipt_no
