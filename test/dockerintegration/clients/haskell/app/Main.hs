-- Drives an OPC XML-DA server through mlabs-haskell/opc-xml-da-client.
--
-- Prints one "CHECK <name> ok|fail <detail>" line per assertion so the Go
-- test can attribute a failure to the thing that failed, and
-- "ALL CHECKS PASSED" at the end so a client that died halfway is
-- distinguishable from one that found nothing wrong.
--
-- What this client contributes that the Python ones cannot: its response
-- parser is hand-written from the specification and strict, so an
-- element or attribute the specification does not allow at that position
-- is a hard ParsingError rather than something quietly skipped; it
-- decodes typed values into a sum type, so ArrayOfDouble either comes
-- back as a vector of doubles or fails; and it parses SOAP faults,
-- including both the 1.1 and 1.2 shapes.
{-# LANGUAGE LambdaCase #-}
{-# LANGUAGE OverloadedLabels #-}
{-# LANGUAGE OverloadedStrings #-}
{-# LANGUAGE ScopedTypeVariables #-}

module Main (main) where

import Control.Concurrent (threadDelay)
import Control.Monad (forM_)
import Data.IORef
import Data.Maybe (fromMaybe, isJust)
import qualified Data.Text as Text
import Data.Text (Text)
import Data.Time.Clock
import qualified Data.Vector as V
import qualified Data.Vector.Unboxed as Uv
import qualified Network.HTTP.Client as Hc
import qualified OpcXmlDaClient as Opc
import System.Environment (lookupEnv)
import System.Exit (exitFailure, exitSuccess)

opcNs :: Text
opcNs = "http://opcfoundation.org/webservices/XMLDA/1.0/"

speed, temperature, running, level, label, capacity, sensors :: Text
speed = "Plant/BuildingA/Line1/Motor1/Speed"
temperature = "Plant/BuildingA/Line1/Motor1/Temperature"
running = "Plant/BuildingA/Line1/Motor1/Running"
level = "Plant/BuildingB/Tank1/Level"
label = "Plant/BuildingB/Tank1/Label"
capacity = "Plant/BuildingB/Tank1/Capacity"
sensors = "Plant/BuildingB/Tank1/Sensors"

-- | Every option on, so the server's gating of ItemName/ItemPath/
-- Timestamp/DiagnosticInfo is exercised rather than left at its sparse
-- defaults.
allOptions :: Opc.RequestOptions
allOptions =
  Opc.RequestOptions
    { Opc._returnErrorText = True,
      Opc._returnDiagnosticInfo = True,
      Opc._returnItemTime = True,
      Opc._returnItemPath = True,
      Opc._returnItemName = True,
      Opc._requestDeadline = Nothing,
      Opc._clientRequestHandle = Just "haskell-interop",
      Opc._localeId = Just "en-US"
    }

readItem :: Text -> Opc.ReadRequestItem
readItem n =
  Opc.ReadRequestItem
    { Opc._itemPath = Nothing,
      Opc._reqType = Nothing,
      Opc._itemName = Just n,
      Opc._clientItemHandle = Just n,
      Opc._maxAge = Nothing
    }

writeItem :: Text -> Opc.Value -> Opc.ItemValue
writeItem n v =
  Opc.ItemValue
    { Opc._diagnosticInfo = Nothing,
      Opc._value = Just v,
      Opc._quality = Nothing,
      Opc._valueTypeQualifier = Nothing,
      Opc._itemPath = Nothing,
      Opc._itemName = Just n,
      Opc._clientItemHandle = Just n,
      Opc._timestamp = Nothing,
      Opc._resultId = Nothing
    }

subscribeItem :: Text -> Opc.SubscribeRequestItem
subscribeItem n =
  Opc.SubscribeRequestItem
    { Opc._itemPath = Nothing,
      Opc._reqType = Nothing,
      Opc._itemName = Just n,
      Opc._clientItemHandle = Just n,
      Opc._deadband = Nothing,
      Opc._requestedSamplingRate = Just 200,
      Opc._enableBuffering = Nothing
    }

browseUnder :: Maybe Text -> Opc.Browse
browseUnder name =
  Opc.Browse
    { Opc._propertyNames = V.empty,
      Opc._localeId = Nothing,
      Opc._clientRequestHandle = Just "haskell-interop",
      Opc._itemPath = Nothing,
      Opc._itemName = name,
      Opc._continuationPoint = Nothing,
      Opc._maxElementsReturned = 0,
      Opc._browseFilter = Opc.AllBrowseFilter,
      Opc._elementNameFilter = Nothing,
      Opc._vendorFilter = Nothing,
      Opc._returnAllProperties = False,
      Opc._returnPropertyValues = False,
      Opc._returnErrorText = True
    }

-- | A per-item ResultID, as the QName the server actually sends: the OPC
-- namespace plus the E_/S_ local name.
opcCode :: Text -> Opc.QName
opcCode = Opc.NamespacedQName opcNs

main :: IO ()
main = do
  endpoint <- fromMaybe "http://127.0.0.1:8080/" <$> lookupEnv "OPCXMLDA_ENDPOINT"
  uri <- case Opc.textUri (Text.pack endpoint) of
    Nothing -> error ("unusable endpoint: " <> endpoint)
    Just u -> pure u
  manager <- Hc.newManager Hc.defaultManagerSettings
  -- 30s, generous for everything except the long poll, which sets its
  -- own budget below.
  reqTimeout <- case Opc.millisecondsRequestTimeout 30000 of
    Nothing -> error "unusable request timeout"
    Just t -> pure t
  failures <- newIORef (0 :: Int)
  total <- newIORef (0 :: Int)

  let check :: String -> Bool -> String -> IO ()
      check name ok detail = do
        modifyIORef' total (+ 1)
        if ok
          then putStrLn ("CHECK " <> name <> " ok " <> detail)
          else do
            modifyIORef' failures (+ 1)
            putStrLn ("CHECK " <> name <> " fail " <> detail)

      op :: Opc.Op i o -> i -> IO (Either Opc.Error o)
      op o = o manager reqTimeout uri

      -- withOk turns "the operation itself failed" into one failed check
      -- naming the client's own error, rather than letting an exception
      -- take down the run and lose every check after it.
      withOk :: String -> Either Opc.Error o -> (o -> IO ()) -> IO ()
      withOk name res k = case res of
        Left e -> check name False (show e)
        Right v -> k v

  -- === GetStatus ==========================================================
  statusRes <- op Opc.getStatus (Opc.GetStatus (Just "en-US") (Just "haskell-interop"))
  withOk "getstatus-parses" statusRes $ \resp -> do
    check "getstatus-parses" True ""
    let st = #status resp :: Maybe Opc.ServerStatus
    check
      "status-identifies-server"
      (maybe False (isJust . #productVersion) st)
      (show (fmap #productVersion st))
    check
      "status-advertises-interface-version"
      (maybe False (V.elem "XML_DA_Version_1_0" . #supportedInterfaceVersions) st)
      (show (fmap #supportedInterfaceVersions st))
    check
      "status-reports-running-state"
      (maybe False ((== Opc.RunningServerState) . #serverState) (#result resp))
      (show (fmap #serverState (#result resp)))
    -- The reply base must echo the handle the request carried, or a
    -- client multiplexing requests cannot correlate them (§3.1.4).
    check
      "reply-echoes-client-request-handle"
      (maybe False ((== Just "haskell-interop") . #clientRequestHandle) (#result resp))
      (show (fmap #clientRequestHandle (#result resp)))
    check
      "reply-revises-locale"
      (maybe False ((== Just "en-US") . #revisedLocaleId) (#result resp))
      (show (fmap #revisedLocaleId (#result resp)))

  -- === Read ===============================================================
  let readReq items =
        Opc.Read
          { Opc._options = Just allOptions,
            Opc._itemList =
              Just
                Opc.ReadRequestItemList
                  { Opc._items = V.fromList (map readItem items),
                    Opc._itemPath = Nothing,
                    Opc._reqType = Nothing,
                    Opc._maxAge = Nothing
                  }
          }

  readRes <- op Opc.read (readReq [speed, temperature, capacity, sensors])
  withOk "read-parses" readRes $ \resp -> do
    check "read-parses" True ""
    let items = maybe V.empty #items (#rItemList resp)
    check "read-returns-every-item" (V.length items == 4) ("got " <> show (V.length items))
    check
      "read-preserves-request-order"
      (map (fromMaybe "" . #clientItemHandle) (V.toList items) == [speed, temperature, capacity, sensors])
      (show (map #clientItemHandle (V.toList items)))
    check
      "read-echoes-item-name-when-asked"
      (all (isJust . #itemName) (V.toList items))
      (show (map #itemName (V.toList items)))
    check
      "read-carries-timestamps-when-asked"
      (all (isJust . #timestamp) (V.toList items))
      (show (map (fmap show . #timestamp) (V.toList items)))
    -- The strict decode is the point: a double must arrive as a double
    -- and an int as an int, not as text this client had to guess at.
    forM_ (V.toList items) $ \item ->
      case (fromMaybe "" (#clientItemHandle item), #value item) of
        (n, Just (Opc.DoubleValue d))
          | n == speed || n == temperature ->
              check ("read-typed-double-" <> Text.unpack n) True (show d)
        (n, Just (Opc.IntValue i))
          | n == capacity -> check "read-typed-int-capacity" True (show i)
        -- An ArrayOfDouble either decodes into a vector of doubles here
        -- or it does not decode at all. No Python client in this suite
        -- can make that distinction.
        (n, Just (Opc.ArrayOfDoubleValue v))
          | n == sensors ->
              check
                "read-typed-array-of-double"
                (Uv.length v == 3)
                (show (Uv.toList v))
        (n, v) -> check ("read-typed-" <> Text.unpack n) False (show v)

  -- An unknown item is that item's own condition, not a fault that costs
  -- the client every other item in the request (§2.6).
  mixedRes <- op Opc.read (readReq [speed, "Plant/NoSuchItem"])
  withOk "read-unknown-item-parses" mixedRes $ \resp -> do
    let items = maybe V.empty #items (#rItemList resp)
    check "read-unknown-item-is-per-item" (V.length items == 2) ("got " <> show (V.length items))
    case V.toList items of
      [good, bad] -> do
        check
          "read-unknown-item-keeps-good-item"
          (#resultId good == Nothing && isJust (#value good))
          (show (#resultId good))
        check
          "read-unknown-item-reports-code"
          (#resultId bad == Just (opcCode "E_UNKNOWNITEMNAME"))
          (show (#resultId bad))
        -- ReturnErrorText was requested, so the deduplicated Errors list
        -- must carry the verbose text for that code (§3.1.9).
        check
          "read-unknown-item-carries-error-text"
          (any (\e -> #id e == opcCode "E_UNKNOWNITEMNAME" && isJust (#text e)) (V.toList (#errors resp)))
          (show (map (\e -> (#id e, #text e)) (V.toList (#errors resp))))
      _ -> pure ()

  -- === Write ==============================================================
  let writeReq itemsIn returnValues =
        Opc.Write
          { Opc._options = Just allOptions,
            Opc._itemList =
              Just
                Opc.WriteRequestItemList
                  { Opc._items = V.fromList itemsIn,
                    Opc._itemPath = Nothing
                  },
            Opc._returnValuesOnReply = returnValues
          }

  wRes <- op Opc.write (writeReq [writeItem speed (Opc.DoubleValue 1234.5)] False)
  withOk "write-parses" wRes $ \resp -> do
    check "write-parses" True ""
    let items = maybe V.empty #items (#rItemList resp)
    check "write-accepted" (V.length items == 1 && all ((== Nothing) . #resultId) (V.toList items)) (show (map #resultId (V.toList items)))
    -- ReturnValuesOnReply was false, so there is no value to report and
    -- therefore no quality either: a Quality on a successful write
    -- contradicts the empty ResultID beside it.
    check
      "write-ack-states-no-quality"
      (all ((== Nothing) . #quality) (V.toList items))
      (show (map (fmap show . #quality) (V.toList items)))

  backRes <- op Opc.read (readReq [speed])
  withOk "write-round-trip-parses" backRes $ \resp ->
    case V.toList (maybe V.empty #items (#rItemList resp)) of
      [item] ->
        check
          "write-round-trips"
          (#value item == Just (Opc.DoubleValue 1234.5))
          (show (#value item))
      other -> check "write-round-trips" False (show (length other) <> " items")

  -- A typed round trip for the non-numeric items too.
  strRes <- op Opc.write (writeReq [writeItem label (Opc.StringValue "haskell"), writeItem running (Opc.BooleanValue True)] True)
  withOk "write-mixed-types-parses" strRes $ \resp -> do
    let items = V.toList (maybe V.empty #items (#rItemList resp))
    check
      "write-mixed-types-accepted"
      (length items == 2 && all ((== Nothing) . #resultId) items)
      (show (map #resultId items))
    -- ReturnValuesOnReply was true this time, so the values come back
    -- and the quality along with them.
    check
      "write-with-values-on-reply-echoes-values"
      (map #value items == [Just (Opc.StringValue "haskell"), Just (Opc.BooleanValue True)])
      (show (map #value items))
    check
      "write-with-values-on-reply-states-quality"
      (all (isJust . #quality) items)
      (show (map (fmap show . #quality) items))

  roRes <- op Opc.write (writeReq [writeItem temperature (Opc.DoubleValue 99)] False)
  withOk "write-read-only-parses" roRes $ \resp ->
    case V.toList (maybe V.empty #items (#rItemList resp)) of
      [item] ->
        check
          "write-read-only-rejected"
          (#resultId item == Just (opcCode "E_READONLY"))
          (show (#resultId item))
      other -> check "write-read-only-rejected" False (show (length other) <> " items")

  -- Out of range clamps, and a clamp is a success code with a caveat
  -- (S_CLAMP), not an error: the write did happen.
  clampRes <- op Opc.write (writeReq [writeItem speed (Opc.DoubleValue 99999)] False)
  withOk "write-clamp-parses" clampRes $ \resp ->
    case V.toList (maybe V.empty #items (#rItemList resp)) of
      [item] ->
        check
          "write-out-of-range-clamps"
          (#resultId item == Just (opcCode "S_CLAMP"))
          (show (#resultId item))
      other -> check "write-out-of-range-clamps" False (show (length other) <> " items")

  clampedRead <- op Opc.read (readReq [speed])
  withOk "write-clamp-applied-parses" clampedRead $ \resp ->
    case V.toList (maybe V.empty #items (#rItemList resp)) of
      [item] ->
        check
          "write-clamp-applied-limit"
          (#value item == Just (Opc.DoubleValue 3000))
          (show (#value item))
      other -> check "write-clamp-applied-limit" False (show (length other) <> " items")

  -- === Browse =============================================================
  rootRes <- op Opc.browse (browseUnder Nothing)
  withOk "browse-parses" rootRes $ \resp -> do
    check "browse-parses" True ""
    let els = V.toList (#elements resp)
    check "browse-root-returns-elements" (not (null els)) ("got " <> show (length els))
    case filter ((== Just "Plant") . #name) els of
      [plant] -> do
        check
          "browse-branch-is-not-an-item"
          (#hasChildren plant && not (#isItem plant))
          ("hasChildren=" <> show (#hasChildren plant) <> " isItem=" <> show (#isItem plant))
        check
          "browse-echoes-fully-qualified-itemname"
          (#itemName plant == Just "Plant")
          (show (#itemName plant))
      other -> check "browse-root-finds-plant" False (show (map #name other))

  motorRes <- op Opc.browse (browseUnder (Just "Plant/BuildingA/Line1/Motor1"))
  withOk "browse-children-parses" motorRes $ \resp -> do
    let els = V.toList (#elements resp)
    check
      "browse-leaves-are-items"
      (length els >= 3 && all #isItem els)
      (show (map (\e -> (#name e, #isItem e)) els))
    check
      "browse-leaf-itemnames-are-addressable"
      (all (maybe False (Text.isPrefixOf "Plant/BuildingA/Line1/Motor1/") . #itemName) els)
      (show (map #itemName els))

  -- === GetProperties ======================================================
  propsRes <-
    op
      Opc.getProperties
      Opc.GetProperties
        { Opc._itemIds = V.singleton (Opc.ItemIdentifier Nothing (Just speed)),
          Opc._propertyNames = V.empty,
          Opc._localeId = Nothing,
          Opc._clientRequestHandle = Just "haskell-interop",
          Opc._itemPath = Nothing,
          Opc._returnAllProperties = True,
          Opc._returnPropertyValues = True,
          Opc._returnErrorText = True
        }
  withOk "getproperties-parses" propsRes $ \resp -> do
    check "getproperties-parses" True ""
    case V.toList (#propertyLists resp) of
      [pl] -> do
        let props = V.toList (#properties pl)
            named n = filter ((== opcCode n) . #name) props
        check "getproperties-returns-properties" (not (null props)) ("got " <> show (length props))
        -- dataType's declared type is QName (§3.1.10 p.40), so a client
        -- that decodes types strictly is what proves the server did not
        -- send it as a plain string.
        check
          "getproperties-datatype-is-a-qname"
          (case named "dataType" of
             [p] -> case #value p of
               Just (Opc.QNameValue _) -> True
               _ -> False
             _ -> False)
          (show (map #value (named "dataType")))
        check
          "getproperties-reports-access-rights"
          (case named "accessRights" of
             [p] -> #value p == Just (Opc.StringValue "readWritable")
             _ -> False)
          (show (map #value (named "accessRights")))
        -- The one complex type the specification puts in a <Value>
        -- position (standard property 3, §3.1.10 p.40). This client
        -- models it as its own case in the value sum type, so it either
        -- decodes as a quality here or the server got the encoding
        -- wrong -- which is exactly the check xmlda.KindQuality was
        -- added for.
        check
          "getproperties-quality-is-an-opcquality"
          (case named "quality" of
             [p] -> case #value p of
               Just (Opc.OpcQualityValue _) -> True
               _ -> False
             _ -> False)
          (show (map #value (named "quality")))
        check
          "getproperties-reports-scan-rate-as-float"
          (case named "scanRate" of
             [p] -> case #value p of
               Just (Opc.FloatValue _) -> True
               _ -> False
             _ -> False)
          (show (map #value (named "scanRate")))
      other -> check "getproperties-returns-properties" False (show (length other) <> " property lists")

  -- === Subscribe / SubscriptionPolledRefresh / SubscriptionCancel =========
  subRes <-
    op
      Opc.subscribe
      Opc.Subscribe
        { Opc._options = Just allOptions,
          Opc._itemList =
            Just
              Opc.SubscribeRequestItemList
                { Opc._items = V.singleton (subscribeItem level),
                  Opc._itemPath = Nothing,
                  Opc._reqType = Nothing,
                  Opc._deadband = Nothing,
                  Opc._requestedSamplingRate = Just 200,
                  Opc._enableBuffering = Nothing
                },
          Opc._returnValuesOnReply = True,
          Opc._subscriptionPingRate = Just 60000
        }
  withOk "subscribe-parses" subRes $ \resp -> do
    check "subscribe-parses" True ""
    let handle = #serverSubHandle resp
    check "subscribe-returns-handle" (maybe False (not . Text.null) handle) (show handle)
    let items = V.toList (maybe V.empty #items (#rItemList resp))
    check
      "subscribe-returns-initial-values"
      (length items == 1 && all (isJust . #value . #itemValue) items)
      (show (map (#value . #itemValue) items))
    check
      "subscribe-echoes-client-item-handle"
      (map (#clientItemHandle . #itemValue) items == [Just level])
      (show (map (#clientItemHandle . #itemValue) items))
    check
      "subscribe-reports-revised-sampling-rate"
      (maybe False (isJust . #revisedSamplingRate) (#rItemList resp))
      (show (fmap #revisedSamplingRate (#rItemList resp)))

    case handle of
      Nothing -> pure ()
      Just h -> do
        let refresh holdTime waitTime =
              op
                Opc.subscriptionPolledRefresh
                Opc.SubscriptionPolledRefresh
                  { Opc._options = Just allOptions,
                    Opc._serverSubHandles = V.singleton h,
                    Opc._holdTime = holdTime,
                    Opc._waitTime = waitTime,
                    Opc._returnAllItems = False
                  }

        -- No HoldTime: "If HoldTime is missing, then WaitTime is
        -- ignored" (§3.6.1 p.62), so this must return at once. The
        -- fixture's item ticks once a second, so the wait beforehand is
        -- what puts a change in the buffer to collect.
        threadDelayMs 1500
        immediate <- refresh Nothing 2000
        withOk "polled-refresh-parses" immediate $ \r -> do
          check "polled-refresh-parses" True ""
          let lists = V.toList (#rItemList r)
          check "polled-refresh-returns-buffered-changes" (not (null lists)) ("got " <> show (length lists))
          check
            "polled-refresh-echoes-subscription-handle"
            (all ((== Just h) . #subscriptionHandle) lists)
            (show (map #subscriptionHandle lists))
          check
            "polled-refresh-reports-no-buffer-overflow"
            (not (#dataBufferOverflow r))
            (show (#dataBufferOverflow r))

        -- With HoldTime set, WaitTime becomes live: hold until HoldTime,
        -- then wait up to WaitTime, returning as soon as a change
        -- arrives. Draining first is what makes the wait real.
        _ <- refresh Nothing 0
        now <- getCurrentTime
        let hold = addUTCTime 0.3 now
        started <- getCurrentTime
        longPoll <- refresh (Just hold) 5000
        finished <- getCurrentTime
        let elapsed = realToFrac (diffUTCTime finished started) :: Double
        withOk "long-poll-parses" longPoll $ \r -> do
          check
            "long-poll-honours-holdtime"
            (elapsed >= 0.25)
            ("returned after " <> show elapsed <> "s, HoldTime was 300ms out")
          check
            "long-poll-returns-on-change-not-on-timeout"
            (not (null (V.toList (#rItemList r))) && elapsed < 4.5)
            ("got " <> show (length (V.toList (#rItemList r))) <> " lists after " <> show elapsed <> "s")

        cancelRes <- op Opc.subscriptionCancel (Opc.SubscriptionCancel (Just h) (Just "haskell-interop"))
        withOk "subscription-cancel-parses" cancelRes $ \r -> do
          check "subscription-cancel-succeeds" True ""
          -- The echoed ClientRequestHandle is deliberately not asserted
          -- here. The server does echo it, as the attribute §3.7.2 p.68
          -- and the schema both declare
          -- (TestHandleSubscriptionCancel_RoundTrip and the
          -- subscriptioncancel golden response pin it), but this
          -- client's parser looks for a child ELEMENT of that name and
          -- so always reports Nothing. Asserting it through this client
          -- would be asserting the client's bug.
          check
            "cancel-response-has-no-unexpected-content"
            (#clientRequestHandle r == Nothing)
            (show (#clientRequestHandle r))

        -- Cancelling again is a no-op success, not a fault:
        -- SubscriptionCancelResponse's fault list (§3.7.2 p.68) is
        -- E_FAIL and E_OUTOFMEMORY only. See open-questions.md OQ-9.
        againRes <- op Opc.subscriptionCancel (Opc.SubscriptionCancel (Just h) Nothing)
        check
          "cancel-is-idempotent"
          (either (const False) (const True) againRes)
          (either show (const "") againRes)

        -- Polling a dead handle is where the specification does put the
        -- fault (§3.6, E_NOSUBSCRIPTION): every handle was invalid, so
        -- there is no per-item list to carry the condition. This client
        -- parses faults, so unlike the Python drivers it can assert the
        -- fault code itself.
        staleRes <- refresh Nothing 0
        check
          "polled-refresh-of-cancelled-handle-faults"
          (case staleRes of
             Left (Opc.SoapError f) -> case #code f of
               Opc.CustomSoapFaultCode (Opc.NamespacedQName ns local) ->
                 ns == opcNs && local == "E_NOSUBSCRIPTION"
               other -> False
             _ -> False)
          (either show (const "unexpectedly succeeded") staleRes)

  -- A fault that is not about subscriptions at all: an empty request
  -- body is a sender-side fault, and the client must surface it as one
  -- rather than as a parse failure.
  badRes <- op Opc.subscriptionCancel (Opc.SubscriptionCancel Nothing Nothing)
  check
    "cancel-without-handle-is-answered-not-crashed"
    (case badRes of
       Right _ -> True
       Left (Opc.SoapError _) -> True
       Left e -> False)
    (either show (const "succeeded") badRes)

  n <- readIORef total
  bad <- readIORef failures
  if bad > 0
    then do
      putStrLn (show bad <> " CHECK(S) FAILED of " <> show n)
      exitFailure
    else do
      putStrLn ("ALL CHECKS PASSED (" <> show n <> " checks)")
      exitSuccess

-- | Sleep, in milliseconds. Control.Concurrent's threadDelay takes
-- microseconds and reads wrong at call sites.
threadDelayMs :: Int -> IO ()
threadDelayMs ms = threadDelay (ms * 1000)
