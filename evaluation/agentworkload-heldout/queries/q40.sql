
SELECT date_trunc('quarter', expense_date) AS quarter,
       count(*) AS receipt_count,
       sum(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= DATE '2026-01-01'
  AND expense_date <= DATE '2026-12-31'
  AND (department = 'Finance' OR department = 'Sales')
GROUP BY date_trunc('quarter', expense_date)
ORDER BY date_trunc('quarter', expense_date)

